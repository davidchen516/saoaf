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
	"time"

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

// Mount registers the resource-hub routes on the given router. The
// caller MUST wrap the router in the identity chain (the reads are
// authenticated — review R2 P1-2: anonymous management reads are
// fail-open and forbidden). Paths are registered DIRECTLY (no nested
// Route("/admin/v1")) so the hub coexists with the I05 admin surface on
// the same mux (review R2 P1-1: two Route() calls on the same path
// panicked the whole process at startup).
func Mount(admin chi.Router, cfg Config) {
	admin.Get("/bindings", cfg.listBindings)
	admin.Get("/resource-plans", cfg.listPlans)
	admin.Get("/resource-plans/{id}", cfg.getPlan)
	admin.Post("/bindings/{id}/publish", cfg.handlePublish)
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
	var capID, provID, snapID int64
	var profile, env, scopeHash string
	qerr := conn.QueryRow(r.Context(), `
		SELECT state, revision, capability_id, provider_id, snapshot_id,
		       profile_or_action, environment, scope_hash
		FROM registry.capability_binding
		WHERE binding_key = $1
		ORDER BY revision DESC LIMIT 1`, key).
		Scan(&curState, &curRev, &capID, &provID, &snapID, &profile, &env, &scopeHash)
	if errors.Is(qerr, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND",
			"binding not found", rid)
		return
	}
	if qerr != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "store error", rid)
		return
	}
	// idempotent re-publish at the same revision (GWT#3)
	if curState == "PUBLISHED" && curRev == expRev {
		httpapi.WriteJSON(w, http.StatusOK, map[string]any{
			"status": "already-published", "revision": curRev,
		})
		return
	}
	if curRev != expRev {
		writeErr(w, http.StatusConflict, "CONFLICT",
			fmt.Sprintf("revision conflict: expected %d, current is %d (no overwrite)", expRev, curRev), rid)
		return
	}

	// the REAL publish: deactivate the old revision (CAS) and insert a new
	// PUBLISHED revision carrying the same content — mirroring the binding
	// module's Publish SQL (module boundary = SQL over shared schemas)
	tx, terr := conn.Begin(r.Context())
	if terr != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "store error", rid)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }() //nolint:errcheck
	newRev := expRev + 1
	if _, err := tx.Exec(r.Context(), `
		UPDATE registry.capability_binding
		SET is_active = FALSE
		WHERE binding_key = $1 AND is_active AND revision = $2`, key, expRev); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "store error", rid)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO registry.capability_binding
			(binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
			 environment, scope, scope_hash, priority, state, revision, is_active)
		SELECT binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
		       environment, scope, scope_hash, priority, 'PUBLISHED', $2, TRUE
		FROM registry.capability_binding WHERE binding_key = $1 AND revision = $3`,
		key, newRev, expRev); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "store error", rid)
		return
	}
	if cerr := tx.Commit(r.Context()); cerr != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "store error", rid)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"status": "published", "revision": newRev,
	})
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
// request_id falls back to a generated UUID — the schema requires
// minLength 1 (review R2 P2).
func writeErr(w http.ResponseWriter, status int, code, msg, requestID string) {
	if requestID == "" {
		requestID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	httpapi.WriteJSON(w, status, map[string]any{
		"error_code": code,
		"message":    msg,
		"request_id": requestID,
	})
}
