// Package hub implements module 03.2 (Resource Hub API) — the Admin API
// surface the management UI reads through (I16). It is a thin READ
// projection over the domain tables: the UI is NOT an authority. Writes do
// NOT mount here — the publish route lives on the I05 admin chain
// (identity → scope → PDP → approval → audit → real domain publish); a
// hub-side publish handler would either bypass those gates or re-implement
// the binding module's invariants (review R3 P1-1/P1-2).
//
// ADR-0006 note: the hub package reads domain tables via SQL over shared
// schemas (the established pattern — binding/resolver/evidence do the
// same); it imports platform packages only.
package hub

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidchen516/saoaf/internal/platform/httpapi"
)

// Config wires the hub read API.
type Config struct {
	Pool *pgxpool.Pool
}

// Mount registers the resource-hub READ routes on the given router. The
// caller MUST wrap the router in the identity chain (the reads are
// authenticated — review R2 P1-2: anonymous management reads are fail-open
// and forbidden). Paths are registered DIRECTLY with relative patterns (no
// nested Route("/admin/v1")) so the hub coexists with the I05 admin surface
// on the same mux (review R2 P1-1), and never re-registers the publish
// route (review R3 P1-1: chi silently overwrites duplicate registrations —
// the gated handler would be stripped; MountAdmin's routeRecorder panics
// on any such attempt).
func Mount(admin chi.Router, cfg Config) {
	admin.Get("/bindings", cfg.listBindings)
	admin.Get("/resource-plans", cfg.listPlans)
	admin.Get("/resource-plans/{id}", cfg.getPlan)
}

func (c Config) listBindings(w http.ResponseWriter, r *http.Request) {
	rows, err := c.Pool.Query(r.Context(), `
		SELECT binding_key, scope_hash, priority, state, revision
		FROM registry.capability_binding
		WHERE is_active
		ORDER BY binding_key LIMIT 200`)
	if err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var key, hash, state string
		var prio, rev int
		if err := rows.Scan(&key, &hash, &prio, &state, &rev); err != nil {
			httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "scan error")
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
	rows, err := c.Pool.Query(r.Context(), `
		SELECT p.id, p.fingerprint, p.status
		FROM resolver.resource_plan p
		ORDER BY p.created_at DESC LIMIT 200`)
	if err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, fp, status string
		if err := rows.Scan(&id, &fp, &status); err != nil {
			httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "scan error")
			return
		}
		out = append(out, map[string]any{
			"id": id, "fingerprint": fp, "status": status, "items": []any{},
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, out)
}

func (c Config) getPlan(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var planID, fp, status string
	err := c.Pool.QueryRow(r.Context(), `
		SELECT id, fingerprint, status FROM resolver.resource_plan
		WHERE id = $1`, id).Scan(&planID, &fp, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.WriteErr(w, r, http.StatusNotFound, "NOT_FOUND", "plan not found")
		return
	}
	if err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
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
