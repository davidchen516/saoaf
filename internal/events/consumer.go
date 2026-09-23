package events

// Backpressure (issue GWT#8) and consumer-side semantics (§5.4):
// at-least-once + CloudEvent id dedup + aggregate revision ordering.

import (
	"context"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BacklogDepth returns the current outbox backlog (PENDING + PUBLISHING).
func BacklogDepth(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var n int64
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM saoaf.outbox_event WHERE status IN ('PENDING','PUBLISHING')`).Scan(&n)
	return n, err
}

// BulkPublishAllowed reports whether a large management publish is
// permitted at the current backlog depth (GWT#8: 超阈值阻止大批量管理
// 发布，已有 Plan 不受影响 — resolve never consults this gate).
func BulkPublishAllowed(depth, threshold int64) bool {
	return depth <= threshold
}

// Deduper absorbs at-least-once duplicates by CloudEvent id
// (GWT#3/GWT#4: 发布成功但标记前崩溃 → 重发 → 消费端幂等去重). Bounded: the
// JetStream duplicate window (2 minutes baseline) covers the crash window;
// the Deduper covers downstream consumers and tests.
type Deduper struct {
	mu   sync.Mutex
	seen map[string]bool
	max  int
}

// NewDeduper builds a bounded deduper.
func NewDeduper(capacity int) *Deduper {
	if capacity <= 0 {
		capacity = 100000
	}
	return &Deduper{seen: make(map[string]bool, capacity/4), max: capacity}
}

// FirstSeen records the id; false means a duplicate (already seen).
func (d *Deduper) FirstSeen(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen[id] {
		return false
	}
	if len(d.seen) >= d.max {
		d.evictOldestLocked()
	}
	d.seen[id] = true
	return true
}

func (d *Deduper) evictOldestLocked() {
	// bounded reset: under adversarial id floods correctness wins over
	// history depth (the transport-level dedup window covers the real
	// crash duplication window)
	d.seen = make(map[string]bool, len(d.seen)/2)
}

// RevisionTracker detects stale revisions per aggregate (§5.4: 同一
// aggregate 使用 revision 检测乱序；旧 revision 可确认并忽略).
type RevisionTracker struct {
	mu    sync.Mutex
	known map[string]int // aggregate -> latest confirmed revision
}

// NewRevisionTracker builds the tracker.
func NewRevisionTracker() *RevisionTracker {
	return &RevisionTracker{known: map[string]int{}}
}

// Confirm applies a revisioned event: returns applied=true when the event
// is new for the aggregate or carries a NEWER revision; false (and
// ignored) when it is a stale/duplicate revision for that aggregate.
func (t *RevisionTracker) Confirm(aggregateKind, aggregateID string, revision int) bool {
	key := aggregateKind + "/" + aggregateID
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur, ok := t.known[key]; ok && revision <= cur {
		return false // 旧 revision：确认并忽略
	}
	t.known[key] = revision
	return true
}

// Ordered applies a batch of revisioned events, dedups by event id,
// drops stale revisions, and returns the applied subset in the ORIGINAL
// arrival order (deterministic: arrival order is the outbox id order).
func Ordered(batch []OutboxRow) []OutboxRow {
	var out []OutboxRow
	tracker := NewRevisionTracker()
	dedup := NewDeduper(len(batch) * 2)
	for _, r := range batch {
		if !dedup.FirstSeen(r.EventID) {
			continue
		}
		if !tracker.Confirm(r.AggregateKind, r.AggregateID, r.AggregateRevision) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// sortRows orders rows by outbox id (stable for tests).
func sortRows(rows []OutboxRow) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
}
