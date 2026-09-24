-- +goose Up
-- I14: Exit Pack Registry（module 03.8，architecture §Exit Pack Registry）。
-- 状态机：DRAFT → VALIDATED（完备性校验）→ ACTIVE（每 vendor 唯一）→
-- {EXPIRED（有效期到期）| SUPERSEDED（新版本替代）}。
-- 不变量：已验证 revision 不可原地修改或删除（trigger）；回滚 = 激活上一
-- 已验证 revision；导出包 = manifest + version + digest 三元组。

CREATE TABLE IF NOT EXISTS saoaf.exit_pack (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  pack_key        TEXT        NOT NULL,           -- vendor 或 vendor:capability 维度键
  vendor          TEXT        NOT NULL,
  revision        INT         NOT NULL CHECK (revision >= 1),
  state           TEXT        NOT NULL DEFAULT 'DRAFT'
                  CHECK (state IN ('DRAFT','VALIDATED','ACTIVE','EXPIRED','SUPERSEDED')),
  owner_ref       TEXT        NOT NULL DEFAULT '',  -- 完备性 1/4
  substitute_provider TEXT    NOT NULL DEFAULT '',  -- 完备性 2/4（替代 Provider）
  recovery_steps  JSONB       NOT NULL DEFAULT '[]', -- 完备性 3/4（导出/恢复步骤）
  evidence_refs   JSONB       NOT NULL DEFAULT '[]', -- 完备性 4/4（有效 Evidence 引用）
  checklist       JSONB       NOT NULL DEFAULT '{}', -- 导出清单（非敏感元数据）
  valid_until     TIMESTAMPTZ,                     -- 有效期（NULL = 永久）
  digest          TEXT,                            -- 导出包 digest（VALIDATED 后填充）
  export_manifest TEXT,                            -- 导出包字节原样存储（digest 对原始字节——JSONB 会变形）
  created_by      TEXT        NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  validated_at    TIMESTAMPTZ,
  activated_at    TIMESTAMPTZ,
  superseded_at   TIMESTAMPTZ,
  expired_at      TIMESTAMPTZ,
  UNIQUE (pack_key, revision)
);
CREATE INDEX IF NOT EXISTS exitpack_active
  ON saoaf.exit_pack (vendor)
  WHERE state = 'ACTIVE';
CREATE INDEX IF NOT EXISTS exitpack_risk
  ON saoaf.exit_pack (state, valid_until)
  WHERE state IN ('EXPIRED','SUPERSEDED');

-- 已验证的 revision 内容不可变（Rollback 条款：历史不可原地修改或删除）。
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION saoaf.freeze_exitpack_content()
RETURNS TRIGGER AS $$
BEGIN
  IF OLD.state IN ('VALIDATED','ACTIVE','EXPIRED','SUPERSEDED') THEN
    IF NEW.owner_ref IS DISTINCT FROM OLD.owner_ref
       OR NEW.substitute_provider IS DISTINCT FROM OLD.substitute_provider
       OR NEW.recovery_steps IS DISTINCT FROM OLD.recovery_steps
       OR NEW.evidence_refs IS DISTINCT FROM OLD.evidence_refs
       OR NEW.checklist IS DISTINCT FROM OLD.checklist
       OR NEW.digest IS DISTINCT FROM OLD.digest
       OR NEW.export_manifest IS DISTINCT FROM OLD.export_manifest THEN
      RAISE EXCEPTION 'verified exit pack revision is immutable' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
DROP TRIGGER IF EXISTS exitpack_immutable ON saoaf.exit_pack;
CREATE TRIGGER exitpack_immutable
  BEFORE UPDATE OR DELETE ON saoaf.exit_pack
  FOR EACH ROW EXECUTE FUNCTION saoaf.freeze_exitpack_content();

GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA saoaf TO saoaf_app;

-- +goose Down
DROP TRIGGER IF EXISTS exitpack_immutable ON saoaf.exit_pack;
DROP FUNCTION IF EXISTS saoaf.freeze_exitpack_content();
DROP TABLE IF EXISTS saoaf.exit_pack;
