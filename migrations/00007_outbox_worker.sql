-- +goose Up
-- I10: Outbox Worker 租约/重试/DLQ 列。半提交防线在 00001（同事务写），
-- 本迁移只加 Worker 侧调度状态（expand 纪律，纯加列）。
-- 语义：PENDING → PUBLISHING（租约）→ PUBLISHED；重试耗尽 → FAILED
-- （DLQ 副本发布到 saoaf.dlq.>，原记录永不删除）。

ALTER TABLE saoaf.outbox_event
  ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS claimed_by      TEXT        NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS next_retry_at  TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS last_error     TEXT        NOT NULL DEFAULT '';

-- Worker 租约引入 PUBLISHING 中间态（领取→发布→标记）；扩展状态 CHECK
--（00001 的原三态约束不认识租约态；expand：仍是终态封闭集）。
ALTER TABLE saoaf.outbox_event DROP CONSTRAINT outbox_event_status_check;
ALTER TABLE saoaf.outbox_event
  ADD CONSTRAINT outbox_event_status_check
  CHECK (status IN ('PENDING','PUBLISHING','PUBLISHED','FAILED'));

-- 可领取扫描：未终态的行（PENDING 立即可领；PUBLISHING 由查询按
-- lease_expires_at 过期判定）。now() 非 IMMUTABLE 不能进索引谓词，
-- 租约过期过滤在查询 WHERE 中执行。
CREATE INDEX IF NOT EXISTS outbox_claimable
  ON saoaf.outbox_event (id)
  WHERE status IN ('PENDING','PUBLISHING');

-- 延迟重试扫描
CREATE INDEX IF NOT EXISTS outbox_retry
  ON saoaf.outbox_event (next_retry_at)
  WHERE status = 'PENDING' AND next_retry_at IS NOT NULL;

-- DLQ 记录（FAILED 行）审计视图：14 天窗口的运营台账
CREATE INDEX IF NOT EXISTS outbox_dlq
  ON saoaf.outbox_event (status, created_at)
  WHERE status = 'FAILED';

GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA saoaf TO saoaf_app;

-- +goose Down
-- 前置条件（审查 R1 P3-5）：Down 恢复原三态 CHECK 时不得存在 PUBLISHING 行
-- （00001 约束不识别租约态）——回退前须停 Worker 并等待在飞租约结算，
-- 或手工将 PUBLISHING 行复位为 PENDING。
ALTER TABLE saoaf.outbox_event DROP CONSTRAINT outbox_event_status_check;
ALTER TABLE saoaf.outbox_event
  ADD CONSTRAINT outbox_event_status_check
  CHECK (status IN ('PENDING','PUBLISHED','FAILED'));
DROP INDEX IF EXISTS saoaf.outbox_dlq;
DROP INDEX IF EXISTS saoaf.outbox_retry;
DROP INDEX IF EXISTS saoaf.outbox_claimable;
ALTER TABLE saoaf.outbox_event
  DROP COLUMN IF EXISTS last_error,
  DROP COLUMN IF EXISTS next_retry_at,
  DROP COLUMN IF EXISTS claimed_by,
  DROP COLUMN IF EXISTS lease_expires_at;
