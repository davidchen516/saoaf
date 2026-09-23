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
