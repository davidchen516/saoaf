-- +goose Up
-- I07: Registry 表——capability/resource_provider/provider_snapshot + 状态机
-- CHECK + 唯一约束 + active pointer 部分唯一索引 + 不可变 trigger。
-- Schema 名按设计基线为 ai_resource_resolver（specs §1），但 I04 已建
-- registry schema（ADR-0006 布局），沿用 registry 以避免双 schema。

CREATE TABLE IF NOT EXISTS registry.capability_definition (
  id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  capability_key     TEXT        NOT NULL,
  major_version      INT         NOT NULL CHECK (major_version >= 1),
  revision           INT         NOT NULL CHECK (revision >= 1),
  resource_type      TEXT        NOT NULL CHECK (resource_type IN ('MODEL','CONTEXT','TOOL','AGENT')),
  requirement_schema JSONB       NOT NULL DEFAULT '{}',
  state              TEXT        NOT NULL DEFAULT 'DRAFT'
                     CHECK (state IN ('DRAFT','VALIDATED','PUBLISHED','DEPRECATED','SUSPENDED','RETIRED')),
  owner_ref          TEXT        NOT NULL,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at       TIMESTAMPTZ,
  tenant_ref         TEXT        NOT NULL DEFAULT '',
  UNIQUE (capability_key, major_version, revision)
);
CREATE INDEX IF NOT EXISTS capability_by_state_type
  ON registry.capability_definition (state, resource_type);

CREATE TABLE IF NOT EXISTS registry.resource_provider (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  provider_key    TEXT        NOT NULL,
  provider_type   TEXT        NOT NULL CHECK (provider_type IN ('MODEL','CONTEXT','TOOL','AGENT')),
  endpoint_ref    TEXT        NOT NULL,
  owner_ref       TEXT        NOT NULL,
  workload_identity TEXT      NOT NULL,
  state           TEXT        NOT NULL DEFAULT 'DRAFT'
                  CHECK (state IN ('DRAFT','VALIDATED','PUBLISHED','DEPRECATED','SUSPENDED','RETIRED')),
  revision        INT         NOT NULL DEFAULT 1 CHECK (revision >= 1),
  active_revision INT         NOT NULL DEFAULT 1,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at    TIMESTAMPTZ,
  tenant_ref      TEXT        NOT NULL DEFAULT '',
  UNIQUE (provider_key, revision)
);
CREATE INDEX IF NOT EXISTS provider_by_type_state
  ON registry.resource_provider (provider_type, state);
-- active pointer: per provider exactly one PUBLISHED row may be pointed to;
-- enforced at application layer via CAS (revision compare-and-swap).

CREATE TABLE IF NOT EXISTS registry.provider_snapshot (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  provider_id       BIGINT      NOT NULL REFERENCES registry.resource_provider(id),
  snapshot_version  INT         NOT NULL CHECK (snapshot_version >= 1),
  contract_version  TEXT        NOT NULL,
  digest            TEXT        NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  signature         TEXT        NOT NULL,
  workload_identity TEXT        NOT NULL,
  profiles          JSONB       NOT NULL DEFAULT '[]',
  generated_at      TIMESTAMPTZ NOT NULL,
  valid_until       TIMESTAMPTZ NOT NULL,
  state             TEXT        NOT NULL DEFAULT 'DRAFT'
                    CHECK (state IN ('DRAFT','VALIDATED','PUBLISHED','DEPRECATED','SUSPENDED','RETIRED')),
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  tenant_ref        TEXT        NOT NULL DEFAULT '',
  UNIQUE (provider_id, snapshot_version),
  CHECK (valid_until > generated_at)
);
CREATE INDEX IF NOT EXISTS snapshot_by_provider_state_valid
  ON registry.provider_snapshot (provider_id, state, valid_until);
-- 同 snapshot_version 不同 digest 已由 UNIQUE (provider_id, snapshot_version)
-- 与 digest CHECK 组合承载：重复版本号即冲突（同版本不同 digest 拒绝）。

-- active pointer 表：每 provider 恰一个 active snapshot（部分唯一索引）
CREATE TABLE IF NOT EXISTS registry.provider_active_pointer (
  provider_id     BIGINT PRIMARY KEY REFERENCES registry.resource_provider(id),
  snapshot_id     BIGINT NOT NULL REFERENCES registry.provider_snapshot(id),
  activated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 禁止字段 schema 级拒绝：profiles/requirement_schema 中的禁用键由应用
-- 校验（JSONB 无法 CHECK 内部键），但顶层列没有物理模型/价格/权重列——
-- 任何此类字段 INSERT 因不存在该列而 42703 拒绝（负向测试证明）。

-- 不可变 trigger：已发布行的内容列冻结（state/published_at 可变）
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION registry.freeze_published_content()
RETURNS TRIGGER AS $$
BEGIN
  IF OLD.state IN ('PUBLISHED','DEPRECATED','SUSPENDED','RETIRED') THEN
    IF NEW.endpoint_ref IS DISTINCT FROM OLD.endpoint_ref
       OR NEW.owner_ref IS DISTINCT FROM OLD.owner_ref
       OR NEW.workload_identity IS DISTINCT FROM OLD.workload_identity
       OR NEW.provider_type IS DISTINCT FROM OLD.provider_type
       OR NEW.revision IS DISTINCT FROM OLD.revision THEN
      RAISE EXCEPTION 'published provider revision immutable' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
DROP TRIGGER IF EXISTS provider_published_immutable ON registry.resource_provider;
CREATE TRIGGER provider_published_immutable
  BEFORE UPDATE ON registry.resource_provider
  FOR EACH ROW EXECUTE FUNCTION registry.freeze_published_content();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION registry.freeze_snapshot_content()
RETURNS TRIGGER AS $$
BEGIN
  IF OLD.state IN ('VALIDATED','PUBLISHED','DEPRECATED','SUSPENDED','RETIRED') THEN
    IF NEW.digest IS DISTINCT FROM OLD.digest
       OR NEW.signature IS DISTINCT FROM OLD.signature
       OR NEW.workload_identity IS DISTINCT FROM OLD.workload_identity
       OR NEW.profiles IS DISTINCT FROM OLD.profiles
       OR NEW.snapshot_version IS DISTINCT FROM OLD.snapshot_version
       OR NEW.contract_version IS DISTINCT FROM OLD.contract_version THEN
      RAISE EXCEPTION 'published snapshot immutable' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
DROP TRIGGER IF EXISTS snapshot_published_immutable ON registry.provider_snapshot;
CREATE TRIGGER snapshot_published_immutable
  BEFORE UPDATE ON registry.provider_snapshot
  FOR EACH ROW EXECUTE FUNCTION registry.freeze_snapshot_content();

GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA registry TO saoaf_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA registry
  GRANT SELECT, INSERT, UPDATE ON TABLES TO saoaf_app;

-- +goose Down
DROP TRIGGER IF EXISTS snapshot_published_immutable ON registry.provider_snapshot;
DROP FUNCTION IF EXISTS registry.freeze_snapshot_content();
DROP TRIGGER IF EXISTS provider_published_immutable ON registry.resource_provider;
DROP FUNCTION IF EXISTS registry.freeze_published_content();
DROP TABLE IF EXISTS registry.provider_active_pointer;
DROP INDEX IF EXISTS registry.snapshot_by_provider_state_valid;
DROP TABLE IF EXISTS registry.provider_snapshot;
DROP INDEX IF EXISTS registry.provider_by_type_state;
DROP TABLE IF EXISTS registry.resource_provider;
DROP INDEX IF EXISTS registry.capability_by_state_type;
DROP TABLE IF EXISTS registry.capability_definition;
