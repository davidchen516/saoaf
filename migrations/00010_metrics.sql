-- +goose Up
-- I13: 主权指标、风险台账（module 03.8，architecture §主权指标）。
-- 不变量：指标 = 纯函数(数据集 revision, 公式版本)——固定数据集重跑
-- diff = 0；追溯三元组（输入 revision、公式版本、Evidence 引用）落列；
-- 告警幂等键 (规则, 实体, revision)——重算/重启零重复；历史不被回滚改写。

CREATE TABLE IF NOT EXISTS saoaf.metric_result (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  metric_key      TEXT        NOT NULL,          -- substitution_coverage / vendor_concentration / protocol_compatibility / evidence_completeness / exit_pack_completeness
  dimensions      JSONB       NOT NULL DEFAULT '{}',  -- capability/provider/vendor/environment/tenant 维度过滤
  value           DOUBLE PRECISION,              -- NULL = 显式未知（未知 ≠ 0）
  status          TEXT        NOT NULL
                  CHECK (status IN ('OK','NOT_APPLICABLE','UNKNOWN','INSUFFICIENT_DATA')),
  status_reason   TEXT        NOT NULL DEFAULT '',   -- 空分母/未知供应商/重复 Provider 等语义分类
  dataset_revision BIGINT     NOT NULL,          -- 输入数据集 revision（追溯三元组 1/3）
  formula_version TEXT        NOT NULL,          -- 公式版本（追溯三元组 2/3；回滚 = 切回旧版本重算，不改写历史）
  evidence_ref    TEXT        NOT NULL DEFAULT '', -- Evidence 引用（三元组 3/3）
  computed_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- 同 (metric, dims, dataset_revision, formula_version) 的重算覆盖为最新值；
  -- 不同 formula_version 的历史行并存（公式回滚不改写历史）
  UNIQUE (metric_key, dimensions, dataset_revision, formula_version)
);
CREATE INDEX IF NOT EXISTS metric_by_key_time
  ON saoaf.metric_result (metric_key, computed_at DESC);

-- 风险台账：告警幂等键 (rule, entity, dataset_revision)——重算/重启零重复
CREATE TABLE IF NOT EXISTS saoaf.risk_alert (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  rule_key        TEXT        NOT NULL,          -- 风险规则（版本化）
  entity_kind     TEXT        NOT NULL,          -- capability / provider / vendor / tenant
  entity_id       TEXT        NOT NULL,
  dataset_revision BIGINT     NOT NULL,
  severity        TEXT        NOT NULL
                  CHECK (severity IN ('LOW','MEDIUM','HIGH','CRITICAL')),
  detail          JSONB       NOT NULL DEFAULT '{}',
  state           TEXT        NOT NULL DEFAULT 'OPEN'
                  CHECK (state IN ('OPEN','ACKNOWLEDGED','RESOLVED')),
  first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (rule_key, entity_kind, entity_id, dataset_revision)   -- 告警幂等键
);
CREATE INDEX IF NOT EXISTS risk_open
  ON saoaf.risk_alert (state, severity DESC)
  WHERE state = 'OPEN';

GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA saoaf TO saoaf_app;

-- +goose Down
DROP TABLE IF EXISTS saoaf.risk_alert;
DROP TABLE IF EXISTS saoaf.metric_result;
