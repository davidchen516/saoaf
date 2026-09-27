-- +goose Up
-- I23: Evidence WORM archive chain (03.8 evidence pack admission).
-- Pack state machine: PENDING -> WRITING -> LOCKED -> VERIFIED; failures
-- enter RETRYABLE or QUARANTINED. Locked objects are never deleted or
-- shortened (WORM invariant enforced by the object store's Object Lock
-- COMPLIANCE mode; this schema tracks the pack ledger and the index
-- linkage).
CREATE TABLE IF NOT EXISTS saoaf.evidence_archive_pack (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  pack_id         TEXT        NOT NULL UNIQUE,          -- idempotent logical pack (content-derived)
  state           TEXT        NOT NULL DEFAULT 'PENDING'
                  CHECK (state IN ('PENDING','WRITING','LOCKED','VERIFIED','RETRYABLE','QUARANTINED')),
  bucket          TEXT        NOT NULL,
  object_key      TEXT        NOT NULL,                 -- manifest object key
  object_version  TEXT        NOT NULL DEFAULT '',      -- S3 version id (set when written)
  manifest_digest TEXT        NOT NULL DEFAULT '',      -- sha256:... over canonical manifest bytes
  retention_until TIMESTAMPTZ,                          -- set at LOCK; only extendable
  legal_hold      BOOLEAN     NOT NULL DEFAULT FALSE,
  record_count    INT         NOT NULL DEFAULT 0,       -- evidence records in the pack
  first_record_id BIGINT     NOT NULL DEFAULT 0,        -- idempotency window: [first, last]
  last_record_id  BIGINT     NOT NULL DEFAULT 0,
  error_reason    TEXT        NOT NULL DEFAULT '',      -- RETRYABLE/QUARANTINED classification
  attempt_count   INT         NOT NULL DEFAULT 0,
  created_by      TEXT        NOT NULL DEFAULT 'worker',
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  locked_at       TIMESTAMPTZ,
  verified_at     TIMESTAMPTZ
);

-- Index linkage: every archived evidence record points at the locked
-- object version. The online index must NEVER reference a missing or
-- unlocked object version (verified by the VERIFIED transition).
CREATE TABLE IF NOT EXISTS saoaf.evidence_archive_link (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  pack_id         TEXT        NOT NULL,
  record_id       BIGINT     NOT NULL,                  -- saoaf.evidence_record.id
  object_version  TEXT        NOT NULL,
  digest          TEXT        NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  archived_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (record_id)                                   -- one pack per record (no double-archive)
);
CREATE INDEX IF NOT EXISTS evidence_archive_link_by_pack
  ON saoaf.evidence_archive_link (pack_id);

-- Worker checkpoint (forward-only, at-least-once scanning + DB dedup).
CREATE TABLE IF NOT EXISTS saoaf.evidence_archive_checkpoint (
  consumer_id     TEXT        NOT NULL PRIMARY KEY,
  last_record_id  BIGINT     NOT NULL,
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
