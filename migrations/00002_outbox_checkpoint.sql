-- +goose Up
-- I04 expand 演示：纯加法（向后兼容）——上一版本应用（只认 v1 形状）继续可读写。
-- outbox 水位：published_seq 单调发布序号（I10 Worker 的 checkpoint 依据）+ 时间索引。

ALTER TABLE saoaf.outbox_event
  ADD COLUMN published_seq BIGINT;

ALTER TABLE saoaf.outbox_event
  ADD CONSTRAINT published_seq_nonnegative CHECK (published_seq IS NULL OR published_seq >= 0);

CREATE UNIQUE INDEX IF NOT EXISTS outbox_published_seq_idx
  ON saoaf.outbox_event (published_seq) WHERE published_seq IS NOT NULL;

CREATE INDEX IF NOT EXISTS outbox_published_at_idx
  ON saoaf.outbox_event (published_at) WHERE status = 'PUBLISHED';

-- +goose Down
DROP INDEX IF EXISTS saoaf.outbox_published_at_idx;
DROP INDEX IF EXISTS saoaf.outbox_published_seq_idx;
ALTER TABLE saoaf.outbox_event
  DROP CONSTRAINT IF EXISTS published_seq_nonnegative;
ALTER TABLE saoaf.outbox_event
  DROP COLUMN IF EXISTS published_seq;
