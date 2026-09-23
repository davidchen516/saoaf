-- +goose Up
-- I11: MMR 父子决策关联证据台账 + 灰度状态机（specs §3/§5：五结局双 ID
-- 关联；shadow → 单 Agent 灰度 → 全量，回退按 Agent/租户切旧静态配置）。
-- 归 saoaf 公共机制 schema（证据台账为跨模块消费，I12 Evidence Index 接线）。

CREATE TABLE IF NOT EXISTS saoaf.model_route_correlation (
  id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  resource_plan_id      TEXT        NOT NULL,
  resource_plan_item_id TEXT        NOT NULL,
  model_route_decision_id TEXT      NOT NULL,
  outcome               TEXT        NOT NULL
                        CHECK (outcome IN ('SUCCEEDED','FALLBACK','QUOTA','TIMEOUT','FAILED')),
  usage_ref             TEXT        NOT NULL DEFAULT '',
  trace_id              TEXT        NOT NULL DEFAULT '',
  recorded_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (resource_plan_item_id, model_route_decision_id)
);
CREATE INDEX IF NOT EXISTS correlation_by_plan
  ON saoaf.model_route_correlation (resource_plan_id, outcome);
CREATE INDEX IF NOT EXISTS correlation_by_decision
  ON saoaf.model_route_correlation (model_route_decision_id);

-- 灰度路由模式：当前状态（变更经 saoaf.change_record 审计；历史不删）。
-- scope: ('' , '') = 全局默认；租户级 (tenant, '')；Agent 级 (tenant, agent)。
CREATE TABLE IF NOT EXISTS saoaf.mmr_routing_mode (
  scope_tenant TEXT        NOT NULL,
  scope_agent  TEXT        NOT NULL DEFAULT '',
  mode         TEXT        NOT NULL
               CHECK (mode IN ('SHADOW','GRAY','FULL','ROLLED_BACK')),
  static_profile_ref TEXT  NOT NULL DEFAULT '',
  updated_by   TEXT        NOT NULL,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (scope_tenant, scope_agent)
);
-- INSERT ... ON CONFLICT 语义见 store（模式变更只审计不删历史行——审计
-- 保留经 change_record）。

GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA saoaf TO saoaf_app;

-- +goose Down
DROP TABLE IF EXISTS saoaf.mmr_routing_mode;
DROP TABLE IF EXISTS saoaf.model_route_correlation;
