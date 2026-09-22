# NATS JetStream event mock

Run `docker compose -f mocks/events/compose.yaml up`. The mock binds client and monitoring ports to loopback and persists JetStream data in a local volume.

The one-shot `init-stream` service creates `SAOAF_EVENTS` from `stream.json`. Local replication is one; production uses a three-node cluster and `num_replicas=3`. The application still uses a transactional PostgreSQL outbox, publishes CloudEvents to `saoaf.>`, and deduplicates by event `id` plus aggregate revision.

This local mock intentionally has no authentication because it is reachable only on loopback. Production requires TLS and NKey/JWT or workload identity, separate publish/consume accounts, subject-level permissions, three replicas, monitoring authentication, and encrypted persistent volumes.
