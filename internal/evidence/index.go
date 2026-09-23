// Package evidence implements the Evidence Index and event-consumption
// recovery (I12, module 03.8, mvp-baseline §7):
//
//   - 幂等消费: evidence_record.event_id UNIQUE absorbs duplicate delivery;
//     the checkpoint is monotonic (only forward) and replay relies on
//     idempotency, never on rewinding
//   - 关联: each consumed event is LINKED to its aggregate reference
//     (plan.resolved → resolver.resource_plan, binding/provider/policy →
//     their registry rows); broken links, unknown topics, malformed or
//     forbidden-content payloads land in QUARANTINED with an alarm
//   - 恢复: two crash windows — evidence-tx commit 前 (replay reprocesses,
//     dedup absorbs) and 提交后/checkpoint 前 (replay rescans the same
//     batch, dedup absorbs, checkpoint re-advances)
//   - 红线: Evidence content is the minimal event payload; prompts, model
//     responses, tool parameters and business bodies are scanned at ingest
//     and quarantined on sight
//
// Module boundary (ADR-0006): SQL over shared schemas only.
package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Evidence states (issue 状态机).
const (
	StateLinked      = "LINKED"
	StateQuarantined = "QUARANTINED"
)

// Quarantine reasons.
const (
	ReasonUnknownTopic     = "UNKNOWN_TOPIC"
	ReasonBrokenLink       = "BROKEN_LINK"
	ReasonForbiddenField   = "FORBIDDEN_CONTENT"
	ReasonMalformedPayload = "MALFORMED_PAYLOAD"
)

// forbiddenContent is the red-line scan on evidence content (common
// delivery constraints: no prompts/model responses/tool params/agent
// messages/business bodies in the control plane).
var forbiddenContent = regexp.MustCompile(
	`(?i)(prompt|model[_-]?response|tool[_-]?param|agent[_-]?message|credential|secret|password|api[_-]?key)`)

// Retention classes (已批准保留与驻留基线).
const (
	RetentionPlanItem   = "PLAN_ITEM"     // 90d online / daily pack 1y
	RetentionDecision   = "DECISION"      // 1y
	RetentionChange     = "CHANGE"        // 2y
	RetentionSnapshot   = "SNAPSHOT_META" // digest + refs 1y
	RetentionEventAudit = "EVENT_AUDIT"   // DB 14d / audit summary only
)

// retentionFor maps a source topic to its retention class.
func retentionFor(topic string) (string, time.Duration) {
	switch {
	case topic == "plan.resolved":
		return RetentionPlanItem, 90 * 24 * time.Hour
	case topic == "binding.published" || topic == "binding.suspended" ||
		topic == "binding.resumed" || topic == "binding.deprecated" || topic == "binding.retired":
		return RetentionChange, 2 * 365 * 24 * time.Hour
	case topic == "provider.snapshot-changed":
		return RetentionSnapshot, 365 * 24 * time.Hour
	default:
		return RetentionEventAudit, 14 * 24 * time.Hour
	}
}

// ConsumedEvent is one outbox event pulled for evidence ingestion.
type ConsumedEvent struct {
	OutboxID          int64
	EventID           string
	Topic             string
	Payload           []byte
	AggregateKind     string
	AggregateID       string
	AggregateRevision int
	TenantRef         string
	CreatedAt         time.Time
	PublishedSeq      int64
}

// Record is one evidence row.
type Record struct {
	ID                int64
	EventID           string
	SourceTopic       string
	AggregateKind     string
	AggregateID       string
	AggregateRevision int
	TenantRef         string
	OccurredAt        time.Time
	PayloadDigest     string
	PlanID            string
	State             string
	QuarantineReason  string
}

// Index ingests events and links evidence (the evidence consumer store).
type Index struct{ Pool *pgxpool.Pool }

// IngestBatch processes consumed events: each becomes exactly one
// evidence record (event_id unique absorbs duplicates), linked or
// quarantined. Returns the number of NEW records and the quarantined
// subset. The checkpoint advances in the SAME tx (crash before commit →
// full redo from the old checkpoint; dedup absorbs).
func (ix Index) IngestBatch(ctx context.Context, consumerID string, events []ConsumedEvent) (int, []Record, error) {
	if len(events) == 0 {
		return 0, nil, nil
	}
	tx, err := ix.Pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var created int
	var quarantined []Record
	var maxSeq int64
	for _, ev := range events {
		if ev.PublishedSeq > maxSeq {
			maxSeq = ev.PublishedSeq
		}
		rec, wasNew, err := ix.ingestOne(ctx, tx, ev)
		if err != nil {
			return 0, nil, err
		}
		if wasNew {
			created++
			if rec.State == StateQuarantined {
				quarantined = append(quarantined, rec)
			}
		}
	}
	// checkpoint advances with the batch commit (monotonic forward-only)
	if _, err := tx.Exec(ctx, `
		INSERT INTO saoaf.evidence_checkpoint (consumer_id, last_seq, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (consumer_id) DO UPDATE
		SET last_seq = $2, updated_at = now()
		WHERE saoaf.evidence_checkpoint.last_seq <= $2`,
		consumerID, maxSeq); err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return created, quarantined, nil
}

// ingestOne writes exactly one evidence record (duplicate deliveries are
// absorbed by the event_id unique constraint) after classifying the event:
// forbidden/malformed payloads, unknown topics and broken aggregate links
// quarantine (with the content replaced by a quarantine stub — the red
// line means the offending payload is never stored); anything else LINKS.
func (ix Index) ingestOne(ctx context.Context, tx pgx.Tx, ev ConsumedEvent) (Record, bool, error) {
	var rec Record
	rec.EventID = ev.EventID
	rec.SourceTopic = ev.Topic
	rec.AggregateKind = ev.AggregateKind
	rec.AggregateID = ev.AggregateID
	rec.AggregateRevision = ev.AggregateRevision
	rec.TenantRef = ev.TenantRef
	rec.OccurredAt = ev.CreatedAt

	digest := sha256.Sum256(ev.Payload)
	rec.PayloadDigest = "sha256:" + hex.EncodeToString(digest[:])

	state, reason, planID, content, retention, until := ix.classify(ctx, tx, ev)
	rec.State = state
	rec.QuarantineReason = reason
	rec.PlanID = planID

	tag, err := tx.Exec(ctx, `
		INSERT INTO saoaf.evidence_record
			(event_id, source_topic, aggregate_kind, aggregate_id, aggregate_revision,
			 tenant_ref, occurred_at, payload_digest, content, plan_id,
			 retention_class, retention_until, state, quarantine_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13, $14)
		ON CONFLICT (event_id) DO NOTHING`,
		rec.EventID, rec.SourceTopic, rec.AggregateKind, rec.AggregateID, rec.AggregateRevision,
		rec.TenantRef, rec.OccurredAt, rec.PayloadDigest, content, rec.PlanID,
		retention, until, state, reason)
	if err != nil {
		return rec, false, err
	}
	if tag.RowsAffected() == 0 {
		return rec, false, nil // duplicate delivery absorbed
	}
	var id int64
	if err := tx.QueryRow(ctx,
		`SELECT id FROM saoaf.evidence_record WHERE event_id = $1`, rec.EventID).Scan(&id); err != nil {
		return rec, false, err
	}
	rec.ID = id
	return rec, true, nil
}

// classify applies the acceptance order to one consumed event and returns
// (state, quarantineReason, planLinkage, storedContent, retentionClass,
// retentionUntil). The storedContent is the event payload for clean events
// and a quarantine stub for violations — forbidden payloads are never
// written to the evidence store (GWT#9 红线).
func (ix Index) classify(ctx context.Context, tx pgx.Tx, ev ConsumedEvent) (state, reason, planID string, content []byte, retention string, until time.Time) {
	retention, d := retentionFor(ev.Topic)
	until = ev.CreatedAt.Add(d)
	content = ev.Payload

	// 1. red line: forbidden or malformed payloads quarantine; the payload
	// itself is replaced by a stub (never stored)
	if v := payloadViolation(ev.Payload); v != "" {
		return StateQuarantined, v, ev.AggregateID,
			[]byte(`{"evidence_status":"quarantined","reason":"` + v + `"}`),
			retention, until
	}

	// 2. topic → linkage rules; unknown topics quarantine (GWT#2)
	switch ev.Topic {
	case "plan.resolved":
		planID = ev.AggregateID
		if linkExists(ctx, tx,
			`SELECT EXISTS(SELECT 1 FROM resolver.resource_plan WHERE id = $1)`, ev.AggregateID) {
			return StateLinked, "", planID, content, retention, until
		}
		return StateQuarantined, ReasonBrokenLink, planID, content, retention, until

	case "binding.published", "binding.suspended", "binding.resumed",
		"binding.deprecated", "binding.retired":
		if linkExists(ctx, tx,
			`SELECT EXISTS(SELECT 1 FROM registry.capability_binding WHERE binding_key = $1)`, ev.AggregateID) {
			return StateLinked, "", "", content, retention, until
		}
		return StateQuarantined, ReasonBrokenLink, "", content, retention, until

	case "provider.snapshot-changed":
		if linkExists(ctx, tx,
			`SELECT EXISTS(SELECT 1 FROM registry.resource_provider WHERE provider_key = $1)`, ev.AggregateID) {
			return StateLinked, "", "", content, retention, until
		}
		return StateQuarantined, ReasonBrokenLink, "", content, retention, until

	default:
		return StateQuarantined, ReasonUnknownTopic, "", content, retention, until
	}
}

func linkExists(ctx context.Context, tx pgx.Tx, query, arg string) bool {
	var exists bool
	if err := tx.QueryRow(ctx, query, arg).Scan(&exists); err != nil {
		return false
	}
	return exists
}

// payloadViolation reports why a payload is rejected ("" = clean):
// non-object JSON (MALFORMED_PAYLOAD) or red-line content
// (FORBIDDEN_CONTENT — prompts, model responses, tool params, credentials).
func payloadViolation(raw []byte) string {
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ReasonMalformedPayload
	}
	if forbiddenContent.Match(raw) {
		return ReasonForbiddenField
	}
	return ""
}

// ConsumeBatch scans the outbox past the checkpoint and ingests one batch.
// Returns the events found (before ingestion) for the caller's loop.
func (ix Index) ScanBatch(ctx context.Context, consumerID string, batchSize int) ([]ConsumedEvent, error) {
	var lastSeq int64
	if err := ix.Pool.QueryRow(ctx,
		`SELECT COALESCE(last_seq, 0) FROM saoaf.evidence_checkpoint WHERE consumer_id = $1`,
		consumerID).Scan(&lastSeq); err != nil {
		// no row → start from 0
		lastSeq = 0
	}
	rows, err := ix.Pool.Query(ctx, `
		SELECT id, event_id, topic, payload, aggregate_kind, aggregate_id,
		       aggregate_revision, created_at, published_seq
		FROM saoaf.outbox_event
		WHERE status = 'PUBLISHED' AND published_seq IS NOT NULL AND published_seq > $1
		ORDER BY published_seq
		LIMIT $2`, lastSeq, batchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConsumedEvent
	for rows.Next() {
		var ev ConsumedEvent
		var tenant string
		if err := rows.Scan(&ev.OutboxID, &ev.EventID, &ev.Topic, &ev.Payload,
			&ev.AggregateKind, &ev.AggregateID, &ev.AggregateRevision,
			&ev.CreatedAt, &ev.PublishedSeq); err != nil {
			return nil, err
		}
		_ = tenant
		out = append(out, ev)
	}
	// tenant_ref joins from the change_record (context attribution)
	for i := range out {
		_ = ix.Pool.QueryRow(ctx, `
			SELECT cr.tenant_ref FROM saoaf.change_record cr
			JOIN saoaf.outbox_event oe ON oe.change_record_id = cr.id
			WHERE oe.id = $1`, out[i].OutboxID).Scan(&out[i].TenantRef)
	}
	return out, rows.Err()
}

// RepairQuarantined re-attempts linkage for quarantined records (issue:
// QUARANTINED →（修复后）LINKED). Only records whose quarantine reason is
// now resolvable flip to LINKED; the rest stay quarantined (never deleted).
func (ix Index) RepairQuarantined(ctx context.Context) (int, error) {
	tag, err := ix.Pool.Exec(ctx, `
		UPDATE saoaf.evidence_record e
		SET state = 'LINKED', quarantine_reason = '', updated_at = now()
		WHERE state = 'QUARANTINED' AND quarantine_reason = 'BROKEN_LINK'
		  AND (
		    (e.source_topic = 'plan.resolved'
		     AND EXISTS(SELECT 1 FROM resolver.resource_plan p WHERE p.id = e.aggregate_id))
		 OR (e.source_topic LIKE 'binding.%'
		     AND EXISTS(SELECT 1 FROM registry.capability_binding b WHERE b.binding_key = e.aggregate_id))
		 OR (e.source_topic = 'provider.snapshot-changed'
		     AND EXISTS(SELECT 1 FROM registry.resource_provider r WHERE r.provider_key = e.aggregate_id))
		  )`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Query filters evidence for the query API (GWT#7: tenant isolation is
// the caller's job — the API layer enforces it).
func (ix Index) Query(ctx context.Context, tenant, planID string, since, until time.Time, limit int) ([]Record, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := ix.Pool.Query(ctx, `
		SELECT id, event_id, source_topic, aggregate_kind, aggregate_id, aggregate_revision,
		       tenant_ref, occurred_at, plan_id, state, quarantine_reason
		FROM saoaf.evidence_record
		WHERE ($1 = '' OR tenant_ref = $1)
		  AND ($2 = '' OR plan_id = $2)
		  AND occurred_at >= $3 AND occurred_at <= $4
		ORDER BY aggregate_kind, aggregate_id, aggregate_revision, occurred_at
		LIMIT $5`, tenant, planID, since, until, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.EventID, &r.SourceTopic, &r.AggregateKind,
			&r.AggregateID, &r.AggregateRevision, &r.TenantRef, &r.OccurredAt,
			&r.PlanID, &r.State, &r.QuarantineReason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QuarantineDepth reports the current quarantine backlog (monitoring).
func (ix Index) QuarantineDepth(ctx context.Context) (int64, error) {
	var n int64
	err := ix.Pool.QueryRow(ctx,
		`SELECT count(*) FROM saoaf.evidence_record WHERE state = 'QUARANTINED'`).Scan(&n)
	return n, err
}

// Checkpoint returns the consumer's current checkpoint.
func (ix Index) Checkpoint(ctx context.Context, consumerID string) (int64, error) {
	var seq int64
	err := ix.Pool.QueryRow(ctx,
		`SELECT last_seq FROM saoaf.evidence_checkpoint WHERE consumer_id = $1`, consumerID).Scan(&seq)
	return seq, err
}
