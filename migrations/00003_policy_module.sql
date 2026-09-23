-- +goose Up
-- I06: policy_revision 表 + 同一 Policy Set 恰一个 ACTIVE 的数据库级强制
-- （唯一部分索引），以及引用不可变的 Plan 侧挂账（policy 引用列）。

CREATE TABLE IF NOT EXISTS policy.policy_revision (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  set_id         TEXT        NOT NULL,
  version        INT         NOT NULL CHECK (version >= 1),
  state          TEXT        NOT NULL DEFAULT 'DRAFT'
                 CHECK (state IN ('DRAFT','PUBLISHED','ACTIVATED','SUPERSEDED')),
  content        JSONB       NOT NULL,
  content_digest TEXT        NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at   TIMESTAMPTZ,
  activated_at   TIMESTAMPTZ,
  tenant_ref     TEXT        NOT NULL DEFAULT '',
  UNIQUE (set_id, version)
);

-- 核心不变量：同一 set 恰一个 ACTIVE（issue: 数据库唯一部分索引强制）
CREATE UNIQUE INDEX IF NOT EXISTS policy_single_active_per_set
  ON policy.policy_revision (set_id) WHERE state = 'ACTIVATED';

CREATE INDEX IF NOT EXISTS policy_by_set_version
  ON policy.policy_revision (set_id, version);

-- Resource Plan 侧引用（引用不可变：行不 UPDATE，新 Plan 新行）
-- I09 落地 Plan 主表；此处先落 policy 引用列的规范化形态与样例表，
-- 供 I06 的「历史 Plan 引用原 revision」断言使用。
CREATE TABLE IF NOT EXISTS policy.plan_policy_ref (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  resource_plan_id  TEXT        NOT NULL,
  policy_set_id     TEXT        NOT NULL,
  policy_version    INT         NOT NULL,
  policy_digest     TEXT        NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (resource_plan_id)
);

GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA policy TO saoaf_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA policy
  GRANT SELECT, INSERT, UPDATE ON TABLES TO saoaf_app;

-- +goose Down
DROP TABLE IF EXISTS policy.plan_policy_ref;
DROP INDEX IF EXISTS policy.policy_by_set_version;
DROP INDEX IF EXISTS policy.policy_single_active_per_set;
DROP TABLE IF EXISTS policy.policy_revision;
