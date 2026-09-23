-- +goose Up
-- I04: 权威持久化基础。逻辑 schema 按模块划分（dev-plan §3 / ADR-0006）：
--   policy / registry / resolver / ops 各一个 schema；公共机制（迁移版本、
--   审计、outbox）在 saoaf 基础 schema。
-- 本迁移只建立基础机制表（goose 自己的版本表 + saoaf 公共表）；
--   业务表随 I06–I17 各自的迁移落地（expand/migrate/contract 纪律）。

-- 角色分离（GWT#6）：
--   saoaf_owner    — DDL/迁移专用
--   saoaf_app      — 应用运行（DML only，无 DDL 权限）
-- 本地/CI 单实例同样创建两个角色以保持权限测试一致。
-- +goose StatementBegin
DO $$
BEGIN
  -- 角色是 cluster 级全局对象：NOT EXISTS + CREATE 在两个会话（如并行测试包
  -- 同时 goose up 不同 database）之间存在竞态窗口，败者撞 pg_authid 唯一索引。
  -- 用异常捕获使 DO 块可安全并发重放（review R1 P3-3；语义不变）。
  BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'saoaf_owner') THEN
      CREATE ROLE saoaf_owner LOGIN;
    END IF;
  EXCEPTION WHEN unique_violation THEN NULL;
  END;
  BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'saoaf_app') THEN
      CREATE ROLE saoaf_app LOGIN;
    END IF;
  EXCEPTION WHEN unique_violation THEN NULL;
  END;
END
$$;
-- +goose StatementEnd

CREATE SCHEMA IF NOT EXISTS saoaf;
CREATE SCHEMA IF NOT EXISTS policy;
CREATE SCHEMA IF NOT EXISTS registry;
CREATE SCHEMA IF NOT EXISTS resolver;
CREATE SCHEMA IF NOT EXISTS ops;

GRANT USAGE ON SCHEMA saoaf, policy, registry, resolver, ops TO saoaf_app;

-- saoaf.change_record：审计三元组（身份/租户/trace）与 decision reference
-- 的公共事务写入表。业务行写入与审计行在同一事务（半提交 = 0 不变量）。
CREATE TABLE IF NOT EXISTS saoaf.change_record (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
  tenant_ref      TEXT         NOT NULL,
  actor           TEXT         NOT NULL,
  trace_id        TEXT         NOT NULL,
  entity_kind     TEXT         NOT NULL,
  entity_id       TEXT         NOT NULL,
  operation       TEXT         NOT NULL,
  decision_ref    TEXT,
  summary         JSONB        NOT NULL DEFAULT '{}'::jsonb,
  CHECK (length(tenant_ref) BETWEEN 1 AND 64),
  CHECK (length(actor) BETWEEN 1 AND 128)
);
CREATE INDEX IF NOT EXISTS change_record_entity_idx
  ON saoaf.change_record (entity_kind, entity_id, created_at DESC);

-- saoaf.outbox_event：Transactional Outbox 表（I10 消费）。
--   状态机 PENDING → PUBLISHED；FAILED 在重试耗尽后进入。
--   aggregate + revision 支持乱序回退检测。
CREATE TABLE IF NOT EXISTS saoaf.outbox_event (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
  published_at    TIMESTAMPTZ,
  attempts        INT          NOT NULL DEFAULT 0,
  status          TEXT         NOT NULL DEFAULT 'PENDING'
                                CHECK (status IN ('PENDING','PUBLISHED','FAILED')),
  topic           TEXT         NOT NULL,
  payload         JSONB        NOT NULL,
  change_record_id BIGINT      NOT NULL REFERENCES saoaf.change_record(id),
  event_id        TEXT         NOT NULL UNIQUE,          -- CloudEvents id 幂等键
  aggregate_kind  TEXT         NOT NULL,
  aggregate_id    TEXT         NOT NULL,
  aggregate_revision INT       NOT NULL CHECK (aggregate_revision >= 1),
  UNIQUE (aggregate_kind, aggregate_id, aggregate_revision)
);
CREATE INDEX IF NOT EXISTS outbox_pending_idx
  ON saoaf.outbox_event (status, id) WHERE status = 'PENDING';

-- 默认拒绝：应用角色仅对机制表授予 DML；后续业务表迁移显式授权。
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA saoaf TO saoaf_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA saoaf
  GRANT SELECT, INSERT, UPDATE ON TABLES TO saoaf_app;

-- +goose Down
-- I02/I04 回滚纪律：expand/migrate/contract；Down 仅用于本地开发重置，
-- 生产回滚不执行破坏性 down migration（issue Rollback 条款）。
DROP TABLE IF EXISTS saoaf.outbox_event;
DROP TABLE IF EXISTS saoaf.change_record;
DROP SCHEMA IF EXISTS saoaf CASCADE;
DROP SCHEMA IF EXISTS resolver CASCADE;
DROP SCHEMA IF EXISTS ops CASCADE;
DROP SCHEMA IF EXISTS registry CASCADE;
DROP SCHEMA IF EXISTS policy CASCADE;
