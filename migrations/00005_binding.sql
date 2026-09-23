-- +goose Up
-- I08: capability_binding 表 + revision CAS + 唯一 active + scope 冲突检测。
-- Schema: registry（沿 I04/I07 布局）。设计基线 specs §1.1/1.2。

CREATE TABLE IF NOT EXISTS registry.capability_binding (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  binding_key       TEXT        NOT NULL,
  capability_id     BIGINT      NOT NULL REFERENCES registry.capability_definition(id),
  provider_id       BIGINT      NOT NULL REFERENCES registry.resource_provider(id),
  snapshot_id       BIGINT      NOT NULL REFERENCES registry.provider_snapshot(id),
  profile_or_action TEXT        NOT NULL,
  environment       TEXT        NOT NULL CHECK (environment IN ('production','staging','development')),
  scope             JSONB       NOT NULL,
  scope_hash        TEXT        NOT NULL,
  constraints       JSONB       NOT NULL DEFAULT '{}',
  priority          INT         NOT NULL DEFAULT 100 CHECK (priority >= 1),
  state             TEXT        NOT NULL DEFAULT 'DRAFT'
                    CHECK (state IN ('DRAFT','PUBLISHED','SUSPENDED','DEPRECATED','RETIRED')),
  revision          INT         NOT NULL DEFAULT 1 CHECK (revision >= 1),
  is_active         BOOLEAN     NOT NULL DEFAULT FALSE,
  change_reason     TEXT        NOT NULL DEFAULT '',
  ticket_ref        TEXT        NOT NULL DEFAULT '',
  approval_ref      TEXT        NOT NULL DEFAULT '',
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at      TIMESTAMPTZ,
  tenant_ref        TEXT        NOT NULL DEFAULT '',
  UNIQUE (binding_key, revision)
);
CREATE INDEX IF NOT EXISTS binding_by_state_env
  ON registry.capability_binding (state, environment);
CREATE INDEX IF NOT EXISTS binding_by_scope_hash
  ON registry.capability_binding (scope_hash, state, priority);

-- 唯一 active：每 binding_key 恰一 active revision
CREATE UNIQUE INDEX IF NOT EXISTS binding_single_active
  ON registry.capability_binding (binding_key) WHERE is_active = TRUE;

-- 冲突不变量：同 scope_hash + 同 priority + 环境一致的「在役」Binding 不可并存。
-- 只约束 state='PUBLISHED' AND is_active 的行：被取代的历史 revision 不参与
-- （否则回滚到同 scope+priority 的历史内容会被自己的历史行挡住）；
-- SUSPENDED 的 Binding 释放槽位（GWT#8：暂停后不再在役，替代 Binding 可发布）。
CREATE UNIQUE INDEX IF NOT EXISTS binding_scope_priority_unique
  ON registry.capability_binding (scope_hash, environment, priority)
  WHERE state = 'PUBLISHED' AND is_active = TRUE;

-- GWT#3 重放幂等：发布请求携带 idempotency key 时在同一事务内记录结果；
-- 同 key（且同请求指纹）重放返回既有状态，不产生第二个 active revision。
-- key 重用于不同意图 → 硬错误（BINDING_IDEMPOTENCY_KEY_REUSE）。
CREATE TABLE IF NOT EXISTS registry.publish_idempotency (
  idempotency_key     TEXT PRIMARY KEY,
  binding_key         TEXT NOT NULL,
  resulting_revision  INT  NOT NULL CHECK (resulting_revision >= 1),
  request_fingerprint TEXT NOT NULL,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- scope 列不可变 trigger（scope_hash 由应用计算，同时冻结避免不一致）
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION registry.freeze_binding_content()
RETURNS TRIGGER AS $$
BEGIN
  IF OLD.is_active THEN
    IF NEW.capability_id IS DISTINCT FROM OLD.capability_id
       OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
       OR NEW.snapshot_id IS DISTINCT FROM OLD.snapshot_id
       OR NEW.profile_or_action IS DISTINCT FROM OLD.profile_or_action
       OR NEW.scope IS DISTINCT FROM OLD.scope
       OR NEW.scope_hash IS DISTINCT FROM OLD.scope_hash
       OR NEW.priority IS DISTINCT FROM OLD.priority
       OR NEW.environment IS DISTINCT FROM OLD.environment THEN
      RAISE EXCEPTION 'active binding revision content immutable' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
DROP TRIGGER IF EXISTS binding_active_immutable ON registry.capability_binding;
CREATE TRIGGER binding_active_immutable
  BEFORE UPDATE ON registry.capability_binding
  FOR EACH ROW EXECUTE FUNCTION registry.freeze_binding_content();

GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA registry TO saoaf_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA registry
  GRANT SELECT, INSERT, UPDATE ON TABLES TO saoaf_app;

-- +goose Down
DROP TABLE IF EXISTS registry.publish_idempotency;
DROP TRIGGER IF EXISTS binding_active_immutable ON registry.capability_binding;
DROP FUNCTION IF EXISTS registry.freeze_binding_content();
DROP INDEX IF EXISTS registry.binding_scope_priority_unique;
DROP INDEX IF EXISTS registry.binding_single_active;
DROP INDEX IF EXISTS registry.binding_by_scope_hash;
DROP INDEX IF EXISTS registry.binding_by_state_env;
DROP TABLE IF EXISTS registry.capability_binding;
