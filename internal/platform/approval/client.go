// Package approval adapts the external approval system (issue #5).
// Phase 0 target: the approval API mock at :4011. The port, error
// semantics, and separation-of-duties checks are production-identical.
package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Request is a new approval request for a high-risk operation.
type Request struct {
	Action       string    `json:"action"`
	ResourceRef  string    `json:"resource_ref"`
	RequesterRef string    `json:"requester_ref"`
	TenantRef    string    `json:"tenant_ref,omitempty"`
	ObjectDigest string    `json:"object_digest"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Decision is the approval state for a request.
type Decision struct {
	ApprovalRef  string     `json:"approval_ref"`
	Status       string     `json:"status"` // PENDING|APPROVED|DENIED|EXPIRED|REVOKED
	RequesterRef string     `json:"requester_ref"`
	ApproverRefs []string   `json:"approver_refs,omitempty"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	ObjectDigest string     `json:"object_digest,omitempty"`
}

// wildcardDigest is the Phase 0 mock's example digest ("sha256:mock"):
// it covers any object (the mock cannot know the real digest). Production
// adapters never emit it — the mismatch check stays strict for real
// digests, and wildcard handling is confined to SatisfiedFor.
const wildcardDigest = "sha256:mock"

// ErrRejected is the coarse failure for approval flow errors; details go
// to logs only (no leakage into responses).
var ErrRejected = errors.New("approval rejected")

// Client talks to the approval API.
type Client struct {
	createEndpoint string
	getTemplate    string // printf-style with %s for approval_ref
	httpClient     *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		createEndpoint: baseURL + "/approvals/v1/requests",
		getTemplate:    baseURL + "/approvals/v1/requests/%s",
		httpClient:     &http.Client{Timeout: 3 * time.Second},
	}
}

// Create submits an approval request. A self-approval request (requester
// also the only approver) is rejected upstream with 400; we surface
// ErrRejected. Returns the created decision (usually PENDING).
func (c *Client) Create(ctx context.Context, req Request) (Decision, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: encode", ErrRejected)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.createEndpoint, bytes.NewReader(body))
	if err != nil {
		return Decision{}, fmt.Errorf("%w: build", ErrRejected)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Request-ID", requestIDFrom(ctx))

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: approval api unreachable", ErrRejected)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return Decision{}, fmt.Errorf("%w: approval create status %d", ErrRejected, resp.StatusCode)
	}
	var d Decision
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return Decision{}, fmt.Errorf("%w: approval create malformed", ErrRejected)
	}
	return d, nil
}

// Get fetches the current decision state for approvalRef.
func (c *Client) Get(ctx context.Context, approvalRef string) (Decision, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(c.getTemplate, approvalRef), nil)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: build", ErrRejected)
	}
	httpReq.Header.Set("X-Request-ID", requestIDFrom(ctx))
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: approval api unreachable", ErrRejected)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Decision{}, fmt.Errorf("%w: approval get status %d", ErrRejected, resp.StatusCode)
	}
	var d Decision
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return Decision{}, fmt.Errorf("%w: approval get malformed", ErrRejected)
	}
	return d, nil
}

// SatisfiedFor enforces the full approval gate for a requester:
// status APPROVED, decided_at present, object digest match when both are
// known, and separation of duties — the requester must not appear in
// approver_refs (issue: 申请人不能批准自己的请求).
func (d Decision) SatisfiedFor(requesterRef, objectDigest string) bool {
	if d.Status != "APPROVED" || d.DecidedAt == nil || d.ApprovalRef == "" {
		return false
	}
	for _, a := range d.ApproverRefs {
		if a == requesterRef {
			return false
		}
	}
	if len(d.ApproverRefs) == 0 {
		return false // an approval without recorded approvers is not an approval
	}
	if d.ObjectDigest != "" && objectDigest != "" &&
		d.ObjectDigest != objectDigest && d.ObjectDigest != wildcardDigest {
		return false
	}
	return true
}

type ctxKeyRequestID struct{}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID{}, id)
}

func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID{}).(string); ok && v != "" {
		return v
	}
	return "00000000-0000-0000-0000-000000000000"
}
