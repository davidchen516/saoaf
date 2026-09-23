// Package events implements the Transactional Outbox worker and the
// NATS JetStream event transport (I10, specs §5):
//
//   - 域状态变更与 outbox insert 同事务（半提交 = 0；由 I07/I08/I09 的写路径保证）
//   - Worker 以 FOR UPDATE SKIP LOCKED + 租约批量领取，多 Worker 并发无双重发布
//   - at-least-once 投递；发布成功但标记前崩溃 → 重复发布，消费者以
//     CloudEvent id 幂等吸收
//   - 重试耗尽 → FAILED + DLQ 副本（saoaf.dlq.>），原记录永不删除
//   - 同 aggregate 携带 revision，旧 revision 消费者可确认并忽略
//
// Module boundary (ADR-0006): this package reads/writes the shared saoaf
// schema via SQL only and imports platform packages only.
package events

import (
	"encoding/json"
	"fmt"
	"time"
)

// CloudEvent is the CloudEvents 1.0 envelope actually placed on the wire
// (specs §5.1). Context attributes carry no sensitive content; `data` is
// the minimal outbox payload.
type CloudEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject"`
	Time            string          `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	TenantRef       string          `json:"tenantref,omitempty"`
	Traceparent     string          `json:"traceparent,omitempty"`
	Data            json.RawMessage `json:"data"`
}

// EventSource is the fixed CloudEvents source attribute.
const EventSource = "urn:enterprise:ai-resource-resolver"

// OutboxRow is a claimed outbox event with everything the envelope needs.
type OutboxRow struct {
	ID                int64
	EventID           string
	Topic             string
	Payload           json.RawMessage
	AggregateKind     string
	AggregateID       string
	AggregateRevision int
	TenantRef         string
	CreatedAt         time.Time
	Attempts          int
}

// CloudEventType maps an outbox topic to the event-catalog type
// (specs §5.2): `binding.published` →
// `com.enterprise.ai.resource.binding.published.v1`.
func CloudEventType(topic string) string {
	return "com.enterprise.ai.resource." + topic + ".v1"
}

// Envelope renders the outbox row as a CloudEvent.
func (r OutboxRow) Envelope() *CloudEvent {
	return &CloudEvent{
		SpecVersion:     "1.0",
		ID:              r.EventID,
		Source:          EventSource,
		Type:            CloudEventType(r.Topic),
		Subject:         fmt.Sprintf("%s/%s", r.AggregateKind, r.AggregateID),
		Time:            r.CreatedAt.UTC().Format(time.RFC3339Nano),
		DataContentType: "application/json",
		TenantRef:       r.TenantRef,
		Data:            r.Payload,
	}
}

// SubjectForTopic is the NATS subject for an outbox topic:
// saoaf.<aggregate>.<event> under the fixed saoaf.> space
// (e.g. topic `binding.published` → saoaf.binding.published).
func SubjectForTopic(topic string) string {
	return "saoaf." + topic
}

// DLQSubject is the dead-letter subject for a failed event.
func DLQSubject(topic string) string {
	return "saoaf.dlq." + topic
}
