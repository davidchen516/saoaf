#!/usr/bin/env bash
# I04 PITR drill: real point-in-time recovery against PostgreSQL 18.6.
#
# Timeline:
#   T0  base schema + marker rows            (in base backup)
#   T1  post-backup WAL rows                 (WAL only)
#   T2  DISASTER: DROP TABLE (cascade)       (must NOT appear in recovery)
#   Recovery target = T1+1s. Verify: goose version, row counts (T1 state),
#   constraint spot-check (PITR 核对清单).
#
# Requires: docker, goose CLI on PATH (v3.28.0), psql (in container).
set -euo pipefail
cd "$(dirname "$0")/.."

WORK=/tmp/pitr-drill
PORT_PRIMARY=5434
PORT_RECOVER=5435
DSN="postgres://postgres:postgres@127.0.0.1:${PORT_PRIMARY}/postgres"
GOOSE_BIN="${GOOSE_BIN:-goose}"

rm -rf "$WORK"; mkdir -p "$WORK/archive" "$WORK/base"
# Linux runners keep the host uid on bind mounts; the container's postgres
# (uid 999) must own /archive to run archive_command (Docker Desktop on
# macOS masks ownership, CI Linux does not).
docker run --rm -v "$WORK/archive:/a" alpine:3.22 chown -R 999:999 /a >/dev/null
docker rm -f saoaf-pitr-primary saoaf-pitr-recover >/dev/null 2>&1 || true

echo "== 1/7 start primary with WAL archiving =="
docker run -d --name saoaf-pitr-primary \
  -e POSTGRES_PASSWORD=postgres \
  -v "$WORK/archive:/archive" \
  -p 127.0.0.1:${PORT_PRIMARY}:5432 \
  postgres:18.6 \
  -c wal_level=replica -c archive_mode=on -c archive_command='cp %p /archive/%f' >/dev/null
# pg_isready must probe TCP (-h 127.0.0.1), not the default unix socket:
# the postgres image's first-boot initdb runs a TEMPORARY server that only
# binds the socket — a socket probe reports ready during init and lets goose
# race the restart window (CI flake: "connection reset by peer" on 5434).
# The temp server has listen_addresses='' so the TCP probe only succeeds
# once the final server is really listening.
for i in $(seq 1 60); do docker exec saoaf-pitr-primary pg_isready -U postgres -h 127.0.0.1 >/dev/null 2>&1 && break; sleep 1; done

echo "== 2/7 apply migrations + T0 baseline rows =="
"$GOOSE_BIN" -dir migrations postgres "$DSN" up
psql_primary() { docker exec -i saoaf-pitr-primary psql -U postgres -d postgres -qtA; }
echo "INSERT INTO saoaf.change_record (tenant_ref, actor, trace_id, entity_kind, entity_id, operation) VALUES ('pitr-t0','drill','t0','cap','cap-t0','CREATE');" | psql_primary
echo "INSERT INTO saoaf.outbox_event (topic,payload,change_record_id,event_id,aggregate_kind,aggregate_id,aggregate_revision) SELECT 'pitr.topic','{}',id,'evt-t0','cap','cap-t0',1 FROM saoaf.change_record WHERE entity_id='cap-t0';" | psql_primary
docker exec saoaf-pitr-primary psql -U postgres -c 'CHECKPOINT;' >/dev/null   # flush to make base backup complete

echo "== 3/7 pg_basebackup (T0 snapshot) =="
docker exec -u postgres saoaf-pitr-primary pg_basebackup -D /var/lib/postgresql/basebackup -Fp -Xs >/dev/null
docker cp saoaf-pitr-primary:/var/lib/postgresql/basebackup "$WORK/base" >/dev/null
chmod -R u+rwX,go-rwx "$WORK/base"

echo "== 4/7 T1 rows (WAL-only, the PITR target) =="
echo "INSERT INTO saoaf.change_record (tenant_ref, actor, trace_id, entity_kind, entity_id, operation) VALUES ('pitr-t1','drill','t1','cap','cap-t1','CREATE');" | psql_primary
echo "INSERT INTO saoaf.outbox_event (topic,payload,change_record_id,event_id,aggregate_kind,aggregate_id,aggregate_revision) SELECT 'pitr.topic','{}',id,'evt-t1','cap','cap-t1',1 FROM saoaf.change_record WHERE entity_id='cap-t1';" | psql_primary
echo "UPDATE saoaf.outbox_event SET status='PUBLISHED', published_at=now(), published_seq=1 WHERE event_id='evt-t1';" | psql_primary
T1=$(echo 'SELECT clock_timestamp();' | psql_primary)
echo "recovery target time: $T1"
sleep 2   # commit boundary; disaster strictly after target

echo "== 5/7 DISASTER: drop the audit tables (cascade) =="
echo 'DROP TABLE saoaf.change_record CASCADE;' | psql_primary
echo "rows after disaster: $(echo 'SELECT count(*) FROM saoaf.change_record;' | psql_primary || echo 'GONE')"
# force the disaster WAL segment out to the archive: recovery must replay a
# commit timestamped AFTER the target to confirm it reached the target and
# stop there (before the DROP)
echo 'SELECT pg_switch_wal();' | psql_primary >/dev/null
sleep 2
# fail fast with the primary's logs if the archiver could not write
ARCHIVED=$(ls "$WORK/archive" 2>/dev/null | wc -l | tr -d ' ')
if [ "$ARCHIVED" -eq 0 ]; then
  echo "ARCHIVE IS EMPTY — archive_command failed; primary logs:"
  docker logs saoaf-pitr-primary 2>&1 | grep -iE "archive|FATAL" | tail -10
  exit 1
fi
echo "archived WAL segments: $ARCHIVED"

echo "== 6/7 recover to T1 from base backup + WAL archive =="
rm -rf "$WORK/recovery"; mkdir -p "$WORK/recovery"
# docker cp places the source dir inside the target when it pre-exists
SRC="$WORK/base"; [ -d "$SRC/basebackup" ] && SRC="$SRC/basebackup"
cp -a "$SRC/." "$WORK/recovery/"
# write recovery config while the host user still owns the files
touch "$WORK/recovery/recovery.signal"
cat >> "$WORK/recovery/postgresql.auto.conf" <<EOF
restore_command = 'cp /archive/%f %p'
recovery_target_time = '$T1'
recovery_target_action = 'promote'
EOF
# the container's postgres (uid 999) must own and read the data dir; on
# Linux bind mounts keep the host uid, and the entrypoint chmods PGDATA
# to 0700, so chown to 999 from inside a root container (portable across
# macOS Docker Desktop and Linux CI runners).
docker run --rm -v "$WORK/recovery:/d" alpine:3.22 chown -R 999:999 /d >/dev/null
# postgres:18 image PGDATA is /var/lib/postgresql/18/docker; override it to
# our mounted plain-layout backup directory.
docker run -d --name saoaf-pitr-recover \
  -e PGDATA=/var/lib/postgresql/data \
  -v "$WORK/recovery:/var/lib/postgresql/data" \
  -v "$WORK/archive:/archive:ro" \
  -p 127.0.0.1:${PORT_RECOVER}:5432 \
  postgres:18.6 >/dev/null
for i in $(seq 1 60); do docker exec saoaf-pitr-recover pg_isready -U postgres -h 127.0.0.1 >/dev/null 2>&1 && break; sleep 1; done
if ! docker exec saoaf-pitr-recover pg_isready -U postgres -h 127.0.0.1 >/dev/null 2>&1; then
  echo "recovery container failed to become ready; logs:"
  docker logs saoaf-pitr-recover 2>&1 | tail -25
  echo "PITR DRILL: FAIL"
  exit 1
fi

echo "== 7/7 PITR 核对清单 =="
psql_rec() { docker exec -i saoaf-pitr-recover psql -U postgres -d postgres -qtA; }
V=$(echo 'SELECT max(version_id) FROM goose_db_version;' | psql_rec)
CR=$(echo 'SELECT count(*) FROM saoaf.change_record;' | psql_rec)
OB=$(echo 'SELECT count(*) FROM saoaf.outbox_event;' | psql_rec)
WP=$(echo 'SELECT max(published_seq) FROM saoaf.outbox_event;' | psql_rec)
ENT=$(echo "SELECT count(*) FROM saoaf.change_record WHERE entity_id IN ('cap-t0','cap-t1');" | psql_rec)
echo "schema version : $V (want 5)"
echo "change_record   : $CR rows (want 2 — T0+T1, disaster excluded)"
echo "outbox_event    : $OB rows (want 2)"
echo "outbox watermark: $WP (want 1 — evt-t1 published_seq)"
echo "entities intact : $ENT (want 2)"
# constraint still enforced post-recovery (negative probe gates the drill)
NEG=$(echo "INSERT INTO saoaf.outbox_event (status,topic,payload,change_record_id,event_id,aggregate_kind,aggregate_id,aggregate_revision) VALUES ('BAD','x','{}',(SELECT id FROM saoaf.change_record LIMIT 1),'evt-neg','a','x',1);" | psql_rec 2>&1 || true)
if echo "$NEG" | grep -q 'violates check constraint'; then
  echo "constraint probe: REJECTED (check constraint enforced post-recovery)"
  CONSTRAINT_OK=1
else
  echo "constraint probe: NOT REJECTED — $NEG"
  CONSTRAINT_OK=0
fi
if [ "$V" = "5" ] && [ "$CR" = "2" ] && [ "$OB" = "2" ] && [ "$WP" = "1" ] && [ "$ENT" = "2" ] && [ "$CONSTRAINT_OK" = "1" ]; then
  echo "PITR DRILL: PASS"
  docker rm -f saoaf-pitr-primary saoaf-pitr-recover >/dev/null 2>&1 || true
  exit 0
fi
echo "PITR DRILL: FAIL"
docker logs saoaf-pitr-recover 2>&1 | tail -20 || true
exit 1
