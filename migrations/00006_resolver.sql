-- +goose Up
-- I09: Resource Plan 持久化。resolver schema 于 00001 建立。
-- 设计基线 specs/resource-resolver-api.md §3：Plan 创建后 item 不可变，
-- 状态仅 RESOLVED → {EXPIRED | REVOKED}（§3.2 过期后审计保留期内可查）。
-- 幂等键唯一：caller + Idempotency-Key 24h 窗口（specs §6）。

CREATE TABLE IF NOT EXISTS resolver.resource_plan (
  id              TEXT        PRIMARY KEY,   -- UUIDv7（有序，cursors 稳定）
  caller_ref      TEXT        NOT NULL,
  tenant_ref      TEXT        NOT NULL,
  task_ref        TEXT        NOT NULL DEFAULT '',
  status          TEXT        NOT NULL DEFAULT 'RESOLVED'
                  CHECK (status IN ('RESOLVED','EXPIRED','REVOKED')),
  fingerprint     TEXT        NOT NULL CHECK (fingerprint ~ '^sha256:[0-9a-f]{64}$'),
  request_digest  TEXT        NOT NULL CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
  idempotency_key TEXT        NOT NULL,
  policy_set_id   TEXT        NOT NULL DEFAULT '',
  policy_version  INT         NOT NULL DEFAULT 0,
  trace_id        TEXT        NOT NULL DEFAULT '',
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at      TIMESTAMPTZ NOT NULL,
  UNIQUE (caller_ref, idempotency_key)      -- 幂等键唯一（GWT#3/#4）
);
CREATE INDEX IF NOT EXISTS plan_by_status_expiry
  ON resolver.resource_plan (status, expires_at);
CREATE INDEX IF NOT EXISTS plan_by_fingerprint
  ON resolver.resource_plan (fingerprint);

CREATE TABLE IF NOT EXISTS resolver.resource_plan_item (
  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  plan_id             TEXT        NOT NULL REFERENCES resolver.resource_plan(id),
  requirement_id      TEXT        NOT NULL,
  capability_key      TEXT        NOT NULL,
  major_version       INT         NOT NULL CHECK (major_version >= 1),
  capability_revision INT         NOT NULL CHECK (capability_revision >= 1),
  binding_key         TEXT        NOT NULL,
  binding_revision    INT         NOT NULL CHECK (binding_revision >= 1),
  provider_key        TEXT        NOT NULL,
  snapshot_version    INT         NOT NULL CHECK (snapshot_version >= 1),
  profile_or_action   TEXT        NOT NULL,
  reason_codes        JSONB       NOT NULL,
  UNIQUE (plan_id, requirement_id)
);

-- Plan item 创建后不可修改（核心验收逻辑：Plan item 创建后不可变）。
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION resolver.freeze_plan_item()
RETURNS TRIGGER AS $$
BEGIN
  RAISE EXCEPTION 'resource_plan_item is immutable' USING ERRCODE = '23514';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
DROP TRIGGER IF EXISTS plan_item_immutable ON resolver.resource_plan_item;
CREATE TRIGGER plan_item_immutable
  BEFORE UPDATE OR DELETE ON resolver.resource_plan_item
  FOR EACH ROW EXECUTE FUNCTION resolver.freeze_plan_item();

-- Plan 状态机：仅 RESOLVED → EXPIRED/REVOKED；且只允许生命周期字段变化
-- （fingerprint/request_digest/idempotency_key 等不可变——审计证据）。
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION resolver.enforce_plan_lifecycle()
RETURNS TRIGGER AS $$
BEGIN
  IF OLD.status <> 'RESOLVED' THEN
    RAISE EXCEPTION 'resource_plan status is terminal after %', OLD.status USING ERRCODE = '23514';
  END IF;
  IF NEW.status NOT IN ('EXPIRED','REVOKED') THEN
    RAISE EXCEPTION 'resource_plan may only move RESOLVED -> EXPIRED/REVOKED' USING ERRCODE = '23514';
  END IF;
  IF NEW.fingerprint IS DISTINCT FROM OLD.fingerprint
     OR NEW.request_digest IS DISTINCT FROM OLD.request_digest
     OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
     OR NEW.caller_ref IS DISTINCT FROM OLD.caller_ref
     OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
    RAISE EXCEPTION 'resource_plan decision fields are immutable' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
DROP TRIGGER IF EXISTS plan_lifecycle ON resolver.resource_plan;
CREATE TRIGGER plan_lifecycle
  BEFORE UPDATE ON resolver.resource_plan
  FOR EACH ROW EXECUTE FUNCTION resolver.enforce_plan_lifecycle();

GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA resolver TO saoaf_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA resolver
  GRANT SELECT, INSERT, UPDATE ON TABLES TO saoaf_app;

-- +goose Down
DROP TRIGGER IF EXISTS plan_lifecycle ON resolver.resource_plan;
DROP FUNCTION IF EXISTS resolver.enforce_plan_lifecycle();
DROP TRIGGER IF EXISTS plan_item_immutable ON resolver.resource_plan_item;
DROP FUNCTION IF EXISTS resolver.freeze_plan_item();
DROP TABLE IF EXISTS resolver.resource_plan_item;
DROP TABLE IF EXISTS resolver.resource_plan;
