-- +goose Up
-- I12: Evidence Index（module 03.8，specs mvp-baseline §7）。事件 → Evidence
-- 关联台账 + 消费 checkpoint + 隔离语义。
-- 状态机：消费后 LINKED（关联成功）｜ QUARANTINED（断链/未知类型/敏感内容）；
-- QUARANTINED →（修复后）LINKED。原始事件与 Evidence 记录永不删除。

CREATE TABLE IF NOT EXISTS saoaf.evidence_record (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  event_id          TEXT        NOT NULL UNIQUE,   -- 幂等消费：重复事件唯一约束拦截
  source_topic      TEXT        NOT NULL,
  aggregate_kind    TEXT        NOT NULL,
  aggregate_id      TEXT        NOT NULL,
  aggregate_revision INT        NOT NULL DEFAULT 1,
  tenant_ref        TEXT        NOT NULL DEFAULT '',
  occurred_at       TIMESTAMPTZ NOT NULL,
  ingested_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  payload_digest    TEXT        NOT NULL CHECK (payload_digest ~ '^sha256:[0-9a-f]{64}$'),
  content           JSONB       NOT NULL DEFAULT '{}',  -- 最小事件载荷（禁止正文红线）
  plan_id           TEXT        NOT NULL DEFAULT '',   -- Resource Plan 关联
  retention_class   TEXT        NOT NULL DEFAULT 'STANDARD',
  retention_until   TIMESTAMPTZ,
  worm_status       TEXT        NOT NULL DEFAULT 'NONE'
                    CHECK (worm_status IN ('NONE','PENDING','ARCHIVED')),
  state             TEXT        NOT NULL
                    CHECK (state IN ('LINKED','QUARANTINED')),
  quarantine_reason TEXT        NOT NULL DEFAULT '',
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS evidence_by_plan
  ON saoaf.evidence_record (plan_id, state);
CREATE INDEX IF NOT EXISTS evidence_by_tenant_time
  ON saoaf.evidence_record (tenant_ref, occurred_at);
CREATE INDEX IF NOT EXISTS evidence_by_state
  ON saoaf.evidence_record (state, ingested_at)
  WHERE state = 'QUARANTINED';
CREATE INDEX IF NOT EXISTS evidence_by_aggregate
  ON saoaf.evidence_record (aggregate_kind, aggregate_id, aggregate_revision);

-- 消费 checkpoint：单调递进（只前进），重放依赖幂等而非回退（issue 数据不变量）。
CREATE TABLE IF NOT EXISTS saoaf.evidence_checkpoint (
  consumer_id TEXT        NOT NULL PRIMARY KEY,
  last_seq    BIGINT      NOT NULL,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (last_seq >= 0)
);

GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA saoaf TO saoaf_app;

-- +goose Down
DROP TABLE IF EXISTS saoaf.evidence_checkpoint;
DROP TABLE IF EXISTS saoaf.evidence_record;
