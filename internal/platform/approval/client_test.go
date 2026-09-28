package approval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSatisfiedFor(t *testing.T) {
	now := time.Now()
	base := Decision{
		ApprovalRef:  "approval:1",
		Status:       "APPROVED",
		RequesterRef: "user:alice",
		ApproverRefs: []string{"user:bob"},
		DecidedAt:    &now,
	}

	if !base.SatisfiedFor("user:alice", "") {
		t.Fatal("legit approval rejected")
	}

	// 申请人自批 → 拒绝
	self := base
	self.ApproverRefs = []string{"user:alice"}
	if self.SatisfiedFor("user:alice", "") {
		t.Fatal("self-approval accepted")
	}
	self2 := base
	self2.ApproverRefs = []string{"user:bob", "user:alice"}
	if self2.SatisfiedFor("user:alice", "") {
		t.Fatal("self-approval among approvers accepted")
	}

	// 无 approver 记录 → 拒绝（无审批人的批准不是批准）
	noApprovers := base
	noApprovers.ApproverRefs = nil
	if noApprovers.SatisfiedFor("user:alice", "") {
		t.Fatal("approval without approver refs accepted")
	}

	// 状态非 APPROVED → 拒绝
	for _, status := range []string{"PENDING", "DENIED", "EXPIRED", "REVOKED"} {
		s := base
		s.Status = status
		if s.SatisfiedFor("user:alice", "") {
			t.Fatalf("status %s accepted", status)
		}
	}

	// digest 不匹配 → 拒绝
	mismatch := base
	mismatch.ObjectDigest = "sha256:other"
	if mismatch.SatisfiedFor("user:alice", "sha256:this") {
		t.Fatal("digest mismatch accepted")
	}
	// 未知 digest（空）→ 放行（mock 可不回传）
	unknown := base
	if !unknown.SatisfiedFor("user:alice", "sha256:this") {
		t.Fatal("absent digest treated as mismatch")
	}
}

func TestClientGetAndFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/approvals/v1/requests/approval:1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"approval_ref": "approval:1", "status": "APPROVED",
				"requester_ref": "user:alice", "approver_refs": []string{"user:bob"},
				"decided_at": time.Now().Format(time.RFC3339),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL)

	d, err := c.Get(context.Background(), "approval:1")
	if err != nil || d.Status != "APPROVED" {
		t.Fatalf("d=%+v err=%v", d, err)
	}

	if _, err := c.Get(context.Background(), "approval:missing"); err == nil {
		t.Fatal("missing approval accepted")
	}
	if _, err := c.Get(context.Background(), ""); err == nil {
		t.Fatal("empty ref accepted")
	}
}

// TestSatisfiedForEmptyRequesterRefFailsClosed pins the R2 rework of the
// review-R1 P2-2 defect: an approval decision WITHOUT a recorded
// requester is malformed and must not be satisfiable by ANY subject —
// blue-team verified: reverting to the `!= "" &&` short-circuit makes
// this test red while the rest of the suite stays green.
func TestSatisfiedForEmptyRequesterRefFailsClosed(t *testing.T) {
	decided := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	d := Decision{
		ApprovalRef:  "approval:empty-req",
		Status:       "APPROVED",
		RequesterRef: "", // malformed/downgraded response: no requester recorded
		ApproverRefs: []string{"user:bob"},
		DecidedAt:    &decided,
		ObjectDigest: "sha256:mock",
	}
	if d.SatisfiedFor("user:mallory", "sha256:mock") {
		t.Fatal("empty requester_ref must fail closed (mallory rode a requester-less approval)")
	}
	if d.SatisfiedFor("", "sha256:mock") {
		t.Fatal("empty requester_ref must fail closed even for an empty subject")
	}
	// control leg: a recorded requester still passes
	d.RequesterRef = "user:alice"
	if !d.SatisfiedFor("user:alice", "sha256:mock") {
		t.Fatal("control leg: recorded requester must pass")
	}
}
