// Package audit writes the admin-write audit trail (I05): every protected
// write records (identity, tenant, trace) plus the decision reference in
// saoaf.change_record from the I04 foundation. A Sink failure blocks the
// write (the operation is not "done" until its audit record commits).
package audit

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Entry is one admin-write audit record.
type Entry struct {
	TenantRef   string
	Actor       string
	TraceID     string
	EntityKind  string
	EntityID    string
	Operation   string
	DecisionRef string
}

// Sink persists audit entries.
type Sink interface {
	Write(ctx context.Context, e Entry) error
}

// NoopSink discards entries (unit tests only; never wired in production).
type NoopSink struct{}

func (NoopSink) Write(context.Context, Entry) error { return nil }

// PostgresSink writes to saoaf.change_record (decision_ref column from
// the I04 foundation).
type PostgresSink struct{ DSN string }

func (p PostgresSink) Write(ctx context.Context, e Entry) error {
	conn, err := pgx.Connect(ctx, p.DSN)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, `
		INSERT INTO saoaf.change_record
			(tenant_ref, actor, trace_id, entity_kind, entity_id, operation, decision_ref, summary)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.TenantRef, e.Actor, e.TraceID, e.EntityKind, e.EntityID, e.Operation,
		e.DecisionRef, map[string]any{"decision_ref": e.DecisionRef, "recorded_at": time.Now().UTC()})
	return err
}
