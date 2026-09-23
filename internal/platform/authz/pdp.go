// Package authz adapts the AuthZEN PDP (issue #5). Decisions are evaluated
// via POST /access/v1/evaluation. Transport errors, timeouts, malformed or
// unknown responses DENY by default — fail-closed precedes availability.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Entity references the AuthZEN subject/action/resource shape.
type Entity struct {
	Type       string         `json:"type,omitempty"`
	ID         string         `json:"id"`
	Properties map[string]any `json:"properties,omitempty"`
}

// Evaluation is an AuthZEN access request.
type Evaluation struct {
	Subject  Entity         `json:"subject"`
	Action   Entity         `json:"action"`
	Resource Entity         `json:"resource"`
	Context  map[string]any `json:"context,omitempty"`
}

// Decision is the AuthZEN response subset we consume.
type Decision struct {
	Allow      bool           `json:"decision"`
	Context    map[string]any `json:"context,omitempty"`
	DecisionID string         // derived from context.policy_decision_id
}

// ErrDenied is returned for explicit PDP denials; any transport/parse
// failure ALSO yields ErrDenied (fail closed) with the cause logged only.
var ErrDenied = errors.New("forbidden")

// PDPClient evaluates requests against the configured PDP endpoint.
type PDPClient struct {
	endpoint   string
	httpClient *http.Client
}

func NewPDPClient(endpoint string) *PDPClient {
	return &PDPClient{
		endpoint: endpoint,
		httpClient: &http.Client{
			Timeout: 3 * time.Second, // PDP must answer fast or we fail closed
		},
	}
}

// Evaluate performs an AuthZEN evaluation. A false decision, transport
// error, timeout, non-200, or unparsable body all return ErrDenied —
// never a silent allow (issue: fail-closed 优先于可用性).
func (c *PDPClient) Evaluate(ctx context.Context, ev Evaluation) (Decision, error) {
	body, err := json.Marshal(ev)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: encode evaluation", ErrDenied)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Decision{}, fmt.Errorf("%w: build request", ErrDenied)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", requestIDFrom(ctx))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: pdp unreachable", ErrDenied)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Decision{}, fmt.Errorf("%w: pdp status %d", ErrDenied, resp.StatusCode)
	}
	var d struct {
		Decision bool           `json:"decision"`
		Context  map[string]any `json:"context"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return Decision{}, fmt.Errorf("%w: pdp malformed response", ErrDenied)
	}
	out := Decision{Allow: d.Decision, Context: d.Context}
	if id, ok := d.Context["policy_decision_id"].(string); ok {
		out.DecisionID = id
	}
	if !out.Allow {
		return out, fmt.Errorf("%w: policy denied", ErrDenied)
	}
	return out, nil
}

type ctxKeyRequestID struct{}

// WithRequestID carries the correlation id to the PDP call.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID{}, id)
}

func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID{}).(string); ok && v != "" {
		return v
	}
	return "00000000-0000-0000-0000-000000000000"
}
