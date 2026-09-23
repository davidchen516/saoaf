package events

import (
	"encoding/json"
	"testing"
)

func mkRow(id int64, eventID, topic, aggID string, rev int) OutboxRow {
	return OutboxRow{
		ID: id, EventID: eventID, Topic: topic, AggregateKind: "binding",
		AggregateID: aggID, AggregateRevision: rev,
		Payload: json.RawMessage(`{"x":1}`),
	}
}

// 消费端幂等：同 CloudEvent id 去重（GWT#3）。
func TestDeduper(t *testing.T) {
	d := NewDeduper(10)
	if !d.FirstSeen("e1") {
		t.Fatal("first occurrence flagged as duplicate")
	}
	if d.FirstSeen("e1") {
		t.Fatal("duplicate accepted")
	}
	if !d.FirstSeen("e2") {
		t.Fatal("different id flagged as duplicate")
	}
}

// 乱序检测：同 aggregate 旧 revision 确认并忽略（§5.4）。
func TestRevisionTracker(t *testing.T) {
	tr := NewRevisionTracker()
	if !tr.Confirm("binding", "b1", 3) {
		t.Fatal("first revision not applied")
	}
	if tr.Confirm("binding", "b1", 2) {
		t.Fatal("stale revision applied")
	}
	if !tr.Confirm("binding", "b1", 4) {
		t.Fatal("newer revision not applied")
	}
	if !tr.Confirm("binding", "b2", 1) {
		t.Fatal("different aggregate not applied")
	}
}

// Ordered：重复 id 去重 + 旧 revision 忽略，保持到达序。
func TestOrdered(t *testing.T) {
	batch := []OutboxRow{
		mkRow(1, "e1", "binding.published", "b1", 1),
		mkRow(2, "e2", "binding.published", "b1", 2),
		mkRow(3, "e2", "binding.published", "b1", 2), // dup event id
		mkRow(4, "e3", "binding.published", "b1", 1), // stale revision
		mkRow(5, "e4", "binding.published", "b2", 7),
	}
	got := Ordered(batch)
	if len(got) != 3 {
		t.Fatalf("applied = %d, want 3 (b1 rev1, b1 rev2, b2 rev7 — dup id 与旧 revision 被忽略)", len(got))
	}
	if got[0].EventID != "e1" || got[1].EventID != "e2" || got[2].EventID != "e4" {
		t.Fatalf("order broken: %+v", got)
	}
}

// CloudEvent 映射：topic → catalog type；subject 命名空间。
func TestCloudEventMapping(t *testing.T) {
	if CloudEventType("binding.published") != "com.enterprise.ai.resource.binding.published.v1" {
		t.Fatalf("type mapping: %s", CloudEventType("binding.published"))
	}
	if SubjectForTopic("binding.published") != "saoaf.binding.published" {
		t.Fatalf("subject mapping: %s", SubjectForTopic("binding.published"))
	}
	if DLQSubject("binding.published") != "saoaf.dlq.binding.published" {
		t.Fatalf("dlq subject: %s", DLQSubject("binding.published"))
	}
	r := mkRow(1, "binding:b1:2", "binding.published", "b1", 2)
	ev := r.Envelope()
	if ev.SpecVersion != "1.0" || ev.ID != "binding:b1:2" ||
		ev.Subject != "binding/b1" || string(ev.Data) != `{"x":1}` {
		t.Fatalf("envelope = %+v", ev)
	}
}

// NoopTransport 满足接口。
func TestNoopTransport(t *testing.T) {
	var tr EventTransport = NoopTransport{}
	if err := tr.Publish(t.Context(), mkRow(1, "e", "t", "a", 1).Envelope()); err != nil {
		t.Fatal(err)
	}
	if !tr.Healthy(t.Context()) {
		t.Fatal("noop should be healthy")
	}
	tr.Close()
}
