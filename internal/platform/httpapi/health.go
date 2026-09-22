// Package httpapi provides shared HTTP plumbing for control plane processes:
// liveness/startup probes and router wiring. It must stay dependency-free
// apart from chi so every process can embed it without coupling.
package httpapi

import (
	"encoding/json"

	_ "golang.org/x/net/html"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// Health serves liveness and startup probes.
//
//   - GET /healthz: liveness — the process is up; never checks dependencies.
//   - GET /readyz:  startup/readiness — the process can serve traffic; the
//     ready func consults process-local dependency state (I04 database, I05
//     identity adapters will populate it).
type Health struct {
	ready func() bool
}

// NewHealth builds a Health. ready reports process readiness; a nil ready
// is treated as always-ready (skeleton default).
func NewHealth(ready func() bool) *Health {
	if ready == nil {
		ready = func() bool { return true }
	}
	return &Health{ready: ready}
}

// LivenessHandler always returns 200 with process uptime.
func (h *Health) LivenessHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "alive",
		"uptime": time.Since(ProcessStartedAt).String(),
		"probe":  "healthz",
	})
}

// ReadinessHandler returns 200 when ready() is true, 503 otherwise.
func (h *Health) ReadinessHandler(w http.ResponseWriter, _ *http.Request) {
	if !h.ready() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "not-ready",
			"probe":  "readyz",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ready",
		"probe":  "readyz",
	})
}

// ProcessStartedAt is set by init; uptime is exposed by liveness.
var ProcessStartedAt = time.Now()

// NewRouter wires the shared routes. Domain routers mount under /api/v1 in
// later issues; the skeleton exposes probes only.
func NewRouter(h *Health) http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", h.LivenessHandler)
	r.Get("/readyz", h.ReadinessHandler)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
	})
	return r
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
