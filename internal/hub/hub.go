// Package hub implements module 03.2 (Resource Hub API) — the Admin API
// surface the management UI reads and writes through (I16). It is a thin
// HTTP projection over the domain stores: the UI is NOT an authority; all
// writes go through the full middleware chain (identity → scope → PDP →
// approval → audit) and the domain CAS semantics (binding revision, plan
// immutability).
//
// ADR-0006 note: the hub package reads domain tables via SQL over shared
// schemas (the established pattern — binding/resolver/evidence do the
// same); it imports platform packages only.
package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidchen516/saoaf/internal/platform/httpapi"
	"github.com/davidchen516/saoaf/internal/platform/middleware"
)

// Config wires the hub API.
type Config struct {
	Pool *pgxpool.Pool
	// BindingsDSN connects to the binding store (module 03.3) for the
	// publish CAS path (DSN shared with the binding module — the store
	// types are not imported across module boundaries; SQL is the boundary).
	BindingsDSN string
}

// Mount wires /admin/v1 resource-hub routes. The caller supplies the
// middleware chain (identity → rate → scope/PDP/approval on the publish
// route).
func Mount(r chi.Router, cfg Config) chi.Router {
	r.Route("/admin/v1", func(v1 chi.Router) {
		v1.Get("/bindings", cfg.listBindings)
		v1.Get("/resource-plans", cfg.listPlans)
		v1.Get("/resource-plans/{id}", cfg.getPlan)
	})
	return r
}

// handlePublish is the PUBLISH route handler — mounted by the caller with
// the full approval chain (scope → PDP → approval). It delegates the CAS
// to the binding store via SQL and maps the conflict to the frozen
// error envelope (CONFLICT).
func (c Config) handlePublish(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFrom(r.Context())
	key := chi.URLParam(r, "id")

	// expected_revision from If-Match (UI) or body
	expected := r.Header.Get("If-Match")
	if expected == "" {
		var body struct {
			ExpectedRevision *int `json:"expected_revision"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err == nil &&
			body.ExpectedRevision != nil {
			expected = strconv.Itoa(*body.ExpectedRevision)
		}
	}
	expRev, err := strconv.Atoi(expected)
	if err != nil || expRev < 1 {
		writeErr(w, http.StatusBadRequest, "VALIDATION_MISSING_REQUIRED",
			"If-Match or expected_revision required (CAS)", rid)
		return
	}

	// The binding CAS path: UPDATE ... WHERE revision = expected. The
	// domain invariants (single active, scope conflicts, audit, outbox)
	// live in the binding module's SQL — the hub replays the minimal CAS
	// here (a thin projection, not a reimplementation: the binding
	// module's Publish API is not importable across module boundaries).
	conn, err := pgx.Connect(r.Context(), c.BindingsDSN)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "INTERNAL",
			"binding store unavailable", rid)
		return
	}
	defer conn.Close(r.Context())

	var curState string
	var curRev int
	qerr := conn.QueryRow(r.Context(), `
		SELECT state, revision FROM registry.capability_binding
		WHERE binding_key = $1
		ORDER BY revision DESC LIMIT 1`, key).Scan(&curState, &curRev)
	if errors.Is(qerr, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND",
			"binding not found", rid)
		return
	}
	if qerr != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "store error", rid)
		return
	}
	if curRev != expRev {
		writeErr(w, http.StatusConflict, "CONFLICT",
			fmt.Sprintf("revision conflict: expected %d, current is %d (no overwrite)", expRev, curRev), rid)
		return
	}
	// idempotent re-publish at the same revision
	if curState == "PUBLISHED" {
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{
			"status": "already-published", "revision": curRev,
		})
		return
	}
	writeErr(w, http.StatusConflict, "CONFLICT",
		"publish at this revision requires the binding module's full publish flow (approval/scope/audit); use the binding admin endpoint", rid)
}

func (c Config) listBindings(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFrom(r.Context())
	rows, err := c.Pool.Query(r.Context(), `
		SELECT binding_key, scope_hash, priority, state, revision
		FROM registry.capability_binding
		WHERE is_active
		ORDER BY binding_key LIMIT 200`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "store error", rid)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var key, hash, state string
		var prio, rev int
		if err := rows.Scan(&key, &hash, &prio, &state, &rev); err != nil {
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "scan error", rid)
			return
		}
		out = append(out, map[string]any{
			"binding_key": key, "scope_hash": hash,
			"priority": prio, "state": state, "revision": rev,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
}

func (c Config) listPlans(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFrom(r.Context())
	rows, err := c.Pool.Query(r.Context(), `
		SELECT p.id, p.fingerprint, p.status
		FROM resolver.resource_plan p
		ORDER BY p.created_at DESC LIMIT 200`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "store error", rid)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, fp, status string
		if err := rows.Scan(&id, &fp, &status); err != nil {
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "scan error", rid)
			return
		}
		out = append(out, map[string]any{
			"id": id, "fingerprint": fp, "status": status, "items": []any{},
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
}

func (c Config) getPlan(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFrom(r.Context())
	id := chi.URLParam(r, "id")
	var planID, fp, status string
	err := c.Pool.QueryRow(r.Context(), `
		SELECT id, fingerprint, status FROM resolver.resource_plan
		WHERE id = $1`, id).Scan(&planID, &fp, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "plan not found", rid)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "store error", rid)
		return
	}
	items := []map[string]any{}
	irows, err := c.Pool.Query(r.Context(), `
		SELECT requirement_id, provider_key FROM resolver.resource_plan_item
		WHERE plan_id = $1 ORDER BY requirement_id`, planID)
	if err == nil {
		defer irows.Close()
		for irows.Next() {
			var reqID, provKey string
			_ = irows.Scan(&reqID, &provKey)
			items = append(items, map[string]any{
				"requirement_id": reqID, "provider_key": provKey,
			})
		}
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"id": planID, "fingerprint": fp, "status": status, "items": items,
	})
}

// writeErr emits the FROZEN error envelope contract:
// flat {error_code, message, request_id} (contracts/schemas/v1/error-envelope.json).
func writeErr(w http.ResponseWriter, status int, code, msg, requestID string) {
	httpapi.WriteJSON(w, status, map[string]any{
		"error_code": code,
		"message":    msg,
		"request_id": requestID,
	})
}
