-- +goose Up
-- I15: Exit Drill / Finding / Remediation（module 03.8）。
-- 状态机：DRAFT → APPROVED → SCHEDULED → RUNNING → {SUCCEEDED|FAILED|ABORTED}
-- →（存在 Finding 时）REMEDIATION_OPEN → CLOSED；另允许
-- SCHEDULED → ABORTED 与 REMEDIATION_OPEN → ABORTED（人工终止）。
-- 审批约束：审批人 ≠ 发起人。CLOSED 前置：无未关闭 Finding + 结果 Evidence 完备。

CREATE TABLE IF NOT EXISTS saoaf.exit_drill (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  drill_key       TEXT        NOT NULL UNIQUE,
  vendor          TEXT        NOT NULL,
  exit_pack_key   TEXT        NOT NULL DEFAULT '',     -- I14 关联（可后补）
  initiator       TEXT        NOT NULL,                 -- 发起人（审批人 ≠ 发起人）
  approver        TEXT        NOT NULL DEFAULT '',      -- 审批人（APPROVED 时填）
  state           TEXT        NOT NULL DEFAULT 'DRAFT'
                  CHECK (state IN ('DRAFT','APPROVED','SCHEDULED','RUNNING',
                                   'SUCCEEDED','FAILED','ABORTED','REMEDIATION_OPEN','CLOSED')),
  result_evidence TEXT        NOT NULL DEFAULT '',      -- 结果 Evidence 引用（CLOSED 前置）
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  approved_at     TIMESTAMPTZ,
  scheduled_at    TIMESTAMPTZ,
  started_at      TIMESTAMPTZ,
  finished_at     TIMESTAMPTZ,
  aborted_at      TIMESTAMPTZ,
  remediation_opened_at TIMESTAMPTZ,
  closed_at       TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS drill_by_state ON saoaf.exit_drill (state);

-- Drill 步骤：外部动作的幂等执行记录（每步幂等键 + Worker 租约 + 超时）
CREATE TABLE IF NOT EXISTS saoaf.exit_drill_step (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  drill_key       TEXT        NOT NULL,
  step_no         INT         NOT NULL CHECK (step_no >= 1),
  action          TEXT        NOT NULL,               -- 外部动作描述（非敏感）
  idempotency_key TEXT        NOT NULL,               -- 外部系统按此去重
  state           TEXT        NOT NULL DEFAULT 'PENDING'
                  CHECK (state IN ('PENDING','RUNNING','DONE','FAILED','SKIPPED')),
  lease_expires_at TIMESTAMPTZ,
  claimed_by      TEXT        NOT NULL DEFAULT '',
  attempts        INT         NOT NULL DEFAULT 0,
  timeout_at      TIMESTAMPTZ,                         -- 超时触发（FAILED/重试上限）
  result_evidence TEXT        NOT NULL DEFAULT '',     -- 每步 Evidence 关联
  UNIQUE (drill_key, step_no),
  UNIQUE (drill_key, idempotency_key)
);
CREATE INDEX IF NOT EXISTS drillstep_claimable
  ON saoaf.exit_drill_step (id)
  WHERE state IN ('PENDING','RUNNING');

-- Finding：SUCCEEDED 后的发现问题；Remediation 闭环
CREATE TABLE IF NOT EXISTS saoaf.exit_drill_finding (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  drill_key       TEXT        NOT NULL,
  finding_key     TEXT        NOT NULL,
  description     TEXT        NOT NULL,
  severity        TEXT        NOT NULL
                  CHECK (severity IN ('LOW','MEDIUM','HIGH','CRITICAL')),
  state           TEXT        NOT NULL DEFAULT 'OPEN'
                  CHECK (state IN ('OPEN','RESOLVED')),
  remediation     TEXT        NOT NULL DEFAULT '',     -- 整改说明（RESOLVED 时必填）
  remediation_evidence TEXT  NOT NULL DEFAULT '',
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  resolved_at     TIMESTAMPTZ,
  UNIQUE (drill_key, finding_key)
);
CREATE INDEX IF NOT EXISTS finding_open
  ON saoaf.exit_drill_finding (drill_key)
  WHERE state = 'OPEN';

-- 状态迁移审计（每步一条；监控导出 = SELECT；不可删除）
CREATE TABLE IF NOT EXISTS saoaf.exit_drill_transition (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  drill_key       TEXT        NOT NULL,
  from_state      TEXT        NOT NULL,
  to_state        TEXT        NOT NULL,
  actor           TEXT        NOT NULL,
  at              TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS drilltrans_by_drill
  ON saoaf.exit_drill_transition (drill_key, at);

GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA saoaf TO saoaf_app;

-- +goose Down
DROP TABLE IF EXISTS saoaf.exit_drill_transition;
DROP TABLE IF EXISTS saoaf.exit_drill_finding;
DROP TABLE IF EXISTS saoaf.exit_drill_step;
DROP TABLE IF EXISTS saoaf.exit_drill;
