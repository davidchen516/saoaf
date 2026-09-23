package events

// EventTransport publishes CloudEvents to the messaging platform. NATS is
// the approved baseline (issue: JetStream 2.14.7, stream SAOAF_EVENTS,
// subjects saoaf.>, 7d/10GiB; DLQ 14d) — NATS is NOT authoritative state.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrTransportDown marks a transport that cannot accept publishes (message
// platform outage) — the worker retries with backoff, never drops.
var ErrTransportDown = fmt.Errorf("events: transport down")

// EventTransport is the publish surface (wired by the composition root).
type EventTransport interface {
	// Publish places the event on the wire. Returns ErrTransportDown for
	// outages (retryable) and other errors for terminal rejects.
	Publish(ctx context.Context, ev *CloudEvent) error
	// Healthy reports whether the transport can accept publishes.
	Healthy(ctx context.Context) bool
	Close()
}

// NoopTransport discards events (unit tests / disabled wiring).
type NoopTransport struct{}

func (NoopTransport) Publish(context.Context, *CloudEvent) error { return nil }
func (NoopTransport) Healthy(context.Context) bool               { return true }
func (NoopTransport) Close()                                     {}

// NATSTransport publishes to the fixed stream SAOAF_EVENTS (subjects
// saoaf.>, 7 days / 10 GiB — the approved baseline). Dead-letter COPIES go
// to the same stream under saoaf.dlq.<topic>: the authoritative DLQ is the
// FAILED outbox row (发布后 14 天 DB retention, specs §6 保留表), never the
// message platform.
//
// The duplicate window matches the mocks/events baseline (120s) so a
// crash-window duplicate publish (published but not marked) is absorbed by
// JetStream de-dup on the same CloudEvent id where timing allows; the
// consumer-side dedup remains the authoritative at-least-once guard.
type NATSTransport struct {
	conn *nats.Conn
	js   jetstream.JetStream

	main jetstream.Stream
}

// NATSConfig is the transport wiring.
type NATSConfig struct {
	URL          string // e.g. nats://127.0.0.1:4222 (loopback for local)
	Replicas     int    // 1 locally, 3 in production (issue: 生产三节点)
	MaxAge       time.Duration
	MaxBytes     int64
	DuplicateWin time.Duration
}

// NewNATSTransport connects and ensures the fixed stream exists.
func NewNATSTransport(ctx context.Context, cfg NATSConfig) (*NATSTransport, error) {
	conn, err := nats.Connect(cfg.URL,
		nats.MaxReconnects(-1),
		nats.ReconnectWait(500*time.Millisecond),
	)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(conn)
	if err != nil {
		_ = conn.Drain()
		return nil, err
	}
	t := &NATSTransport{conn: conn, js: js}
	if t.main, err = t.ensureStream(ctx, "SAOAF_EVENTS", []string{"saoaf.>"}, cfg); err != nil {
		_ = conn.Drain()
		return nil, err
	}
	return t, nil
}

func (t *NATSTransport) ensureStream(ctx context.Context, name string, subjects []string, cfg NATSConfig) (jetstream.Stream, error) {
	maxAge := cfg.MaxAge
	if maxAge == 0 {
		maxAge = 7 * 24 * time.Hour
	}
	maxBytes := cfg.MaxBytes
	if maxBytes == 0 {
		maxBytes = 10 << 30 // 10 GiB baseline
	}
	replicas := cfg.Replicas
	if replicas == 0 {
		replicas = 1
	}
	dupWin := cfg.DuplicateWin
	if dupWin == 0 {
		dupWin = 120 * time.Second
	}
	s, err := t.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       name,
		Subjects:   subjects,
		Retention:  jetstream.LimitsPolicy,
		MaxAge:     maxAge,
		MaxBytes:   maxBytes,
		Replicas:   replicas,
		Duplicates: dupWin,
		Storage:    jetstream.FileStorage,
		Discard:    jetstream.DiscardOld,
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Publish places the event on its subject. It waits for the JetStream
// publish ack — a publish that has not been acked is NOT a success.
func (t *NATSTransport) Publish(ctx context.Context, ev *CloudEvent) error {
	if t == nil || t.conn == nil || !t.conn.IsConnected() {
		return ErrTransportDown
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	subject := SubjectForTopic(topicFromType(ev.Type))
	pub, err := t.js.Publish(ctx, subject, body, jetstream.WithMsgID(ev.ID))
	if err != nil {
		return ErrTransportDown // timeouts/disconnects retry; NATS is never terminal
	}
	_ = pub
	return nil
}

// PublishDLQ places the dead-letter copy on the main stream under
// saoaf.dlq.<topic> (best-effort; the authoritative DLQ record is the
// FAILED outbox row, 14d DB retention — never deleted by the worker).
func (t *NATSTransport) PublishDLQ(ctx context.Context, ev *CloudEvent, reason string) error {
	if t == nil || t.conn == nil || !t.conn.IsConnected() {
		return ErrTransportDown
	}
	dlqEvent := *ev
	dlqEvent.Type = ev.Type + ".dead"
	var data map[string]json.RawMessage
	_ = json.Unmarshal(ev.Data, &data)
	data["dlq_reason"], _ = json.Marshal(reason)
	dlqEvent.Data, _ = json.Marshal(data)
	body, err := json.Marshal(&dlqEvent)
	if err != nil {
		return err
	}
	_, err = t.js.Publish(ctx, DLQSubject(topicFromType(ev.Type)), body, jetstream.WithMsgID("dlq:"+ev.ID))
	if err != nil {
		return ErrTransportDown
	}
	return nil
}

// Healthy reports connectivity + stream info readability.
func (t *NATSTransport) Healthy(ctx context.Context) bool {
	if t == nil || t.conn == nil || !t.conn.IsConnected() {
		return false
	}
	_, err := t.main.Info(ctx)
	return err == nil
}

func (t *NATSTransport) Close() {
	if t != nil && t.conn != nil {
		_ = t.conn.Drain()
	}
}

func topicFromType(t string) string {
	// com.enterprise.ai.resource.<topic>.v1 → <topic>
	const prefix = "com.enterprise.ai.resource."
	const suffix = ".v1"
	if len(t) > len(prefix)+len(suffix) && t[:len(prefix)] == prefix {
		return t[len(prefix) : len(t)-len(suffix)]
	}
	return t
}
