// Package ops implements module 03.8 (Sovereignty Operations API) — the
// READ surface the operations UI consumes (I17): sovereignty metrics with
// drill-down triples, risk alerts, evidence break-chain, exit-pack risk
// view, drill state and remediation (findings) — all server-side
// paginated. It follows the I16 hub pattern: a thin SQL projection over
// shared schemas (ADR-0006); it imports platform packages only and mounts
// inside the identity-gated admin subtree.
//
// Semantics this surface must preserve (issue core acceptance logic):
//   - 未知 ≠ 0: metrics with non-OK status carry value=null + status_reason;
//     the UI renders explicit states, never zero-risk.
//   - drill-down triple: every metric view resolves to (formula_version,
//     dataset_revision, evidence_ref).
//   - Drill illegal operations are rejected by the API (the I15 state
//     machine), not by hiding buttons.
//   - Pagination is server-side; the browser never loads full tables.
//
// Authorization: every route requires the "ops.read" scope. Tenant
// isolation: callers without "ops.all-tenants" are confined to their
// trusted tenant_ref (token claim) — tenant-scoped views force the filter
// and off-tenant ?tenant= requests are rejected 403 (issue AC: 跨租户越权
// 读取被拒绝). Environment isolation: a token "environments" claim (empty =
// unrestricted) confines the environment drill-down axis; off-scope
// ?environment= requests are rejected 403 (跨环境越权读取被拒绝).
package ops

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidchen516/saoaf/internal/platform/httpapi"
	"github.com/davidchen516/saoaf/internal/platform/middleware"
)

// Config wires the ops read API.
type Config struct {
	Pool *pgxpool.Pool
}

// Mount registers the ops READ routes (relative paths — the caller mounts
// them inside the identity-gated /admin/v1 subtree, like the I16 hub).
// Every route requires the ops.read scope on top of the identity chain.
func Mount(admin chi.Router, cfg Config) {
	r := admin.With(middleware.RequireScope("ops.read"))
	r.Get("/ops/overview", cfg.overview)
	r.Get("/ops/metrics", cfg.listMetrics)
	r.Get("/ops/metrics/history", cfg.metricHistory)
	r.Get("/ops/metrics/broken", cfg.brokenEvidence)
	r.Get("/ops/alerts", cfg.listAlerts)
	r.Get("/ops/evidence", cfg.listEvidence)
	r.Get("/ops/exit-packs", cfg.listExitPacks)
	r.Get("/ops/drills", cfg.listDrills)
	r.Get("/ops/drills/{id}", cfg.getDrill)
	r.Get("/ops/drills/{id}/log", cfg.drillLog)
}

// scope returns the caller's authorization envelope: tenant confinement
// (empty tenantFilter = no forced filter = cross-tenant allowed) and
// environment confinement (nil envs = unrestricted). An error signals a
// rejected request (off-scope explicit filter — 403, never silently
// scoped: the caller must KNOW the read was refused).
func scope(r *http.Request, q url.Values) (tenantFilter string, envs []string, err bool) {
	id := middleware.IdentityFrom(r.Context())
	if id == nil {
		return "", nil, true
	}
	// environment axis: trusted claim, explicit off-scope request rejected
	envs = id.Environments
	if len(envs) > 0 {
		if e := q.Get("environment"); e != "" && !contains(envs, e) {
			return "", nil, true
		}
	}
	// tenant axis: ops.all-tenants holders see everything; others confined
	if id.HasScope("ops.all-tenants") {
		return "", envs, false
	}
	if t := q.Get("tenant"); t != "" && t != id.TenantRef {
		return "", envs, true // explicit cross-tenant request → reject
	}
	return id.TenantRef, envs, false
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}

// parseLimit reads ?limit= (default 50, max 200 — the single-page payload
// cap; the browser never loads full tables).
func parseLimit(q url.Values) int {
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	return limit
}

func parseOffset(q url.Values) int {
	off, err := strconv.Atoi(q.Get("offset"))
	if err != nil || off < 0 {
		return 0
	}
	return off
}

// overview aggregates the dashboard header counts: open alerts, expired
// packs, active drills, open findings, quarantine depth. Unknowns surface
// as counts with explicit statuses — never folded into "healthy".
func (c Config) overview(w http.ResponseWriter, r *http.Request) {
	var openAlerts, expiredPacks, activeDrills, openFindings, quarantine int64
	unknown := []string{}
	if err := c.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM saoaf.risk_alert WHERE state <> 'RESOLVED'`).Scan(&openAlerts); err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	if err := c.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM saoaf.exit_pack WHERE state = 'ACTIVE' AND valid_until IS NOT NULL AND valid_until < now()`).Scan(&expiredPacks); err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	if err := c.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM saoaf.exit_drill WHERE state IN ('APPROVED','SCHEDULED','RUNNING')`).Scan(&activeDrills); err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	if err := c.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM saoaf.exit_drill_finding WHERE state = 'OPEN'`).Scan(&openFindings); err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	if err := c.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM saoaf.evidence_record WHERE worm_status = 'PENDING'`).Scan(&quarantine); err != nil {
		// evidence schema lives in the same saoaf schema — absence would be
		// a deployment inconsistency, surfaced honestly
		unknown = append(unknown, "quarantine_depth")
	}
	// explicit unknown metrics: non-OK latest rows must NOT read as healthy.
	// A query failure surfaces as an unknown field (null count) — never as 0
	// (review R1 P2-1: this dashboard's own 未知≠0 rule applies to itself).
	unknownMetrics := any(nil)
	var um int64
	if err := c.Pool.QueryRow(r.Context(),
		`SELECT count(*) FROM saoaf.metric_result m
		 WHERE m.computed_at = (SELECT MAX(computed_at) FROM saoaf.metric_result m2
		                        WHERE m2.metric_key = m.metric_key AND m2.dimensions = m.dimensions)
		   AND m.status <> 'OK'`).Scan(&um); err == nil {
		unknownMetrics = um
	} else {
		unknown = append(unknown, "unknown_metrics")
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"open_alerts":      openAlerts,
		"expired_packs":    expiredPacks,
		"active_drills":    activeDrills,
		"open_findings":    openFindings,
		"quarantine_depth": quarantine,
		"unknown_fields":   unknown,
		"unknown_metrics":  unknownMetrics,
		"generated_at":     time.Now().UTC().Format(time.RFC3339),
	})
}

// listMetrics returns the LATEST metric result per (metric_key, dimensions)
// with drill-down filters (metric, capability, provider, vendor,
// environment, tenant — issue: 按 Capability、Provider、Vendor、环境和时间下钻).
// Every row carries the full drill-down triple.
func (c Config) listMetrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset := parseLimit(q), parseOffset(q)
	tenantFilter, envs, rejected := scope(r, q)
	if rejected {
		httpapi.WriteErr(w, r, http.StatusForbidden, "FORBIDDEN",
			"request crosses the caller's tenant/environment scope")
		return
	}

	// DISTINCT ON latest per (metric_key, dimensions), then JSONB dimension
	// filters applied on the deduped set (dimension keys are the canonical
	// drill-down axes; ?environment=prod filters dimensions->>'environment')
	where := "1=1"
	args := []any{limit, offset}
	arg := func(v string) string { args = append(args, v); return strconv.Itoa(len(args)) }
	if v := q.Get("metric"); v != "" {
		where += ` AND m.metric_key = $` + arg(v)
	}
	for _, dim := range []string{"capability", "provider", "vendor", "environment", "tenant"} {
		if v := q.Get(dim); v != "" {
			where += ` AND m.dimensions->>'` + dim + `' = $` + arg(v)
		}
	}
	// tenant confinement: callers without ops.all-tenants see only their
	// tenant's rows (platform-wide rows without a tenant dim stay invisible)
	if tenantFilter != "" {
		where += ` AND m.dimensions->>'tenant' = $` + arg(tenantFilter)
	}
	// environment confinement: rows limited to the trusted environments
	if len(envs) > 0 {
		ph := make([]string, 0, len(envs))
		for _, e := range envs {
			ph = append(ph, "$"+arg(e))
		}
		where += ` AND m.dimensions->>'environment' IN (` + joinComma(ph) + `)`
	}
	// time drill-down: only metric points computed in the window
	if v := q.Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			where += ` AND m.computed_at >= $` + arg(t.Format(time.RFC3339))
		} else {
			httpapi.WriteErr(w, r, http.StatusBadRequest, "VALIDATION_INVALID_ENUM", "since must be RFC3339")
			return
		}
	}

	rows, err := c.Pool.Query(r.Context(), `
		SELECT metric_key, dimensions, value, status, status_reason,
		       dataset_revision, formula_version, evidence_ref, computed_at
		FROM (
			SELECT DISTINCT ON (metric_key, dimensions)
				metric_key, dimensions, value, status, status_reason,
				dataset_revision, formula_version, evidence_ref, computed_at
			FROM saoaf.metric_result m
			ORDER BY metric_key, dimensions, computed_at DESC
		) m
		WHERE `+where+`
		ORDER BY metric_key, dimensions
		LIMIT $1 OFFSET $2`, args...)
	if err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var key, status, reason, formula, evidence string
		var dims map[string]string
		var value *float64
		var rev int64
		var computedAt time.Time
		if err := rows.Scan(&key, &dims, &value, &status, &reason, &rev, &formula, &evidence, &computedAt); err != nil {
			httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "scan error")
			return
		}
		out = append(out, map[string]any{
			"metric_key": key, "dimensions": dims,
			// value stays null for UNKNOWN/INSUFFICIENT_DATA — 未知 ≠ 0
			"value": value, "status": status, "status_reason": reason,
			// the drill-down triple, always complete
			"dataset_revision": rev, "formula_version": formula,
			"evidence_ref": evidence, "computed_at": computedAt,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"items": out, "limit": limit, "offset": offset, "count": len(out),
	})
}

// metricHistory returns every stored point of one (metric, dimensions)
// series — different formula_version rows coexist (公式回滚不改写历史).
func (c Config) metricHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key := q.Get("metric")
	if key == "" {
		httpapi.WriteErr(w, r, http.StatusBadRequest, "VALIDATION_MISSING_REQUIRED", "metric required")
		return
	}
	tenantFilter, envs, rejected := scope(r, q)
	if rejected {
		httpapi.WriteErr(w, r, http.StatusForbidden, "FORBIDDEN",
			"request crosses the caller's tenant/environment scope")
		return
	}
	limit, offset := parseLimit(q), parseOffset(q)
	where := "metric_key = $1"
	args := []any{key, limit, offset}
	if tenantFilter != "" {
		args = append(args, tenantFilter)
		where += ` AND dimensions->>'tenant' = $` + strconv.Itoa(len(args))
	}
	// environment confinement (review R1 P1-1: history previously dropped
	// the envs claim — a production-scoped operator could read staging rows)
	if len(envs) > 0 {
		ph := make([]string, 0, len(envs))
		for _, e := range envs {
			args = append(args, e)
			ph = append(ph, "$"+strconv.Itoa(len(args)))
		}
		where += ` AND dimensions->>'environment' IN (` + joinComma(ph) + `)`
	}
	// the explicit environment filter must also apply (it was silently
	// ignored before — same finding)
	if v := q.Get("environment"); v != "" {
		args = append(args, v)
		where += ` AND dimensions->>'environment' = $` + strconv.Itoa(len(args))
	}
	rows, err := c.Pool.Query(r.Context(), `
		SELECT dimensions, value, status, status_reason, dataset_revision,
		       formula_version, evidence_ref, computed_at
		FROM saoaf.metric_result
		WHERE `+where+`
		ORDER BY computed_at DESC, id DESC
		LIMIT $2 OFFSET $3`, args...)
	if err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var dims map[string]string
		var status, reason, formula, evidence string
		var value *float64
		var rev int64
		var computedAt time.Time
		if err := rows.Scan(&dims, &value, &status, &reason, &rev, &formula, &evidence, &computedAt); err != nil {
			httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "scan error")
			return
		}
		out = append(out, map[string]any{
			"dimensions": dims, "value": value, "status": status,
			"status_reason": reason, "dataset_revision": rev,
			"formula_version": formula, "evidence_ref": evidence,
			"computed_at": computedAt,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"items": out, "limit": limit, "offset": offset, "count": len(out),
	})
}

// brokenEvidence returns metric rows whose evidence chain is BROKEN:
// empty evidence_ref, or a reference that does not resolve to any indexed
// evidence record (issue view: 证据断链). These surface with an explicit
// broken_reason — never as healthy metrics.
func (c Config) brokenEvidence(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset := parseLimit(q), parseOffset(q)
	tenantFilter, envs, rejected := scope(r, q)
	if rejected {
		httpapi.WriteErr(w, r, http.StatusForbidden, "FORBIDDEN",
			"request crosses the caller's tenant/environment scope")
		return
	}
	where := `(m.evidence_ref = '' OR NOT EXISTS (
			SELECT 1 FROM saoaf.evidence_record er
			WHERE er.event_id = m.evidence_ref
			   OR 'saoaf://evidence/' || er.event_id = m.evidence_ref))`
	args := []any{limit, offset}
	if tenantFilter != "" {
		args = append(args, tenantFilter)
		where += ` AND m.dimensions->>'tenant' = $` + strconv.Itoa(len(args))
	}
	// environment confinement (review R1 P1-1: broken previously dropped the
	// envs claim — a production-scoped operator could read staging rows)
	if len(envs) > 0 {
		ph := make([]string, 0, len(envs))
		for _, e := range envs {
			args = append(args, e)
			ph = append(ph, "$"+strconv.Itoa(len(args)))
		}
		where += ` AND m.dimensions->>'environment' IN (` + joinComma(ph) + `)`
	}
	rows, err := c.Pool.Query(r.Context(), `
		SELECT metric_key, dimensions, value, status, status_reason,
		       dataset_revision, formula_version, evidence_ref, computed_at
		FROM (
			SELECT DISTINCT ON (metric_key, dimensions)
				metric_key, dimensions, value, status, status_reason,
				dataset_revision, formula_version, evidence_ref, computed_at
			FROM saoaf.metric_result m
			ORDER BY metric_key, dimensions, computed_at DESC
		) m
		WHERE `+where+`
		ORDER BY metric_key, dimensions
		LIMIT $1 OFFSET $2`, args...)
	if err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var key, status, reason, formula, evidence string
		var dims map[string]string
		var value *float64
		var rev int64
		var computedAt time.Time
		if err := rows.Scan(&key, &dims, &value, &status, &reason, &rev, &formula, &evidence, &computedAt); err != nil {
			httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "scan error")
			return
		}
		why := "evidence_ref does not resolve to an indexed evidence record"
		if evidence == "" {
			why = "evidence_ref is empty"
		}
		out = append(out, map[string]any{
			"metric_key": key, "dimensions": dims, "value": value,
			"status": status, "status_reason": reason,
			"dataset_revision": rev, "formula_version": formula,
			"evidence_ref": evidence, "computed_at": computedAt,
			"broken_reason": why,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"items": out, "limit": limit, "offset": offset, "count": len(out),
	})
}

// listEvidence returns indexed evidence records (I12) — the evidence view
// with the break-chain context (quarantined rows carry their state).
// Tenant-enforced like the metric views.
func (c Config) listEvidence(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset := parseLimit(q), parseOffset(q)
	tenantFilter, _, rejected := scope(r, q)
	if rejected {
		httpapi.WriteErr(w, r, http.StatusForbidden, "FORBIDDEN",
			"request crosses the caller's tenant/environment scope")
		return
	}
	where := "1=1"
	args := []any{limit, offset}
	arg := func(v string) string { args = append(args, v); return strconv.Itoa(len(args)) }
	if tenantFilter != "" {
		where += ` AND er.tenant_ref = $` + arg(tenantFilter)
	} else if t := q.Get("tenant"); t != "" {
		where += ` AND er.tenant_ref = $` + arg(t)
	}
	if v := q.Get("plan_id"); v != "" {
		where += ` AND er.plan_id = $` + arg(v)
	}
	if v := q.Get("since"); v != "" {
		if _, err := time.Parse(time.RFC3339, v); err != nil {
			httpapi.WriteErr(w, r, http.StatusBadRequest, "VALIDATION_INVALID_ENUM", "since must be RFC3339")
			return
		}
		where += ` AND er.occurred_at >= $` + arg(v)
	}
	rows, err := c.Pool.Query(r.Context(), `
		SELECT event_id, source_topic, aggregate_kind, aggregate_id,
		       aggregate_revision, tenant_ref, occurred_at, payload_digest,
		       plan_id, retention_class, retention_until, worm_status
		FROM saoaf.evidence_record er
		WHERE `+where+`
		ORDER BY occurred_at DESC
		LIMIT $1 OFFSET $2`, args...)
	if err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var eventID, topic, kind, aggID, tenant, digest, planID, retention, worm string
		var aggRev int
		var occurredAt time.Time
		var retentionUntil *time.Time
		if err := rows.Scan(&eventID, &topic, &kind, &aggID, &aggRev, &tenant,
			&occurredAt, &digest, &planID, &retention, &retentionUntil, &worm); err != nil {
			httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "scan error")
			return
		}
		out = append(out, map[string]any{
			"event_id": eventID, "source_topic": topic,
			"aggregate_kind": kind, "aggregate_id": aggID,
			"aggregate_revision": aggRev, "tenant_ref": tenant,
			"occurred_at": occurredAt, "payload_digest": digest,
			"plan_id": planID, "retention_class": retention,
			"retention_until": retentionUntil, "worm_status": worm,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"items": out, "limit": limit, "offset": offset, "count": len(out),
	})
}

// listAlerts returns open risk alerts (I13), server-side paginated.
func (c Config) listAlerts(w http.ResponseWriter, r *http.Request) {
	limit, offset := parseLimit(r.URL.Query()), parseOffset(r.URL.Query())
	rows, err := c.Pool.Query(r.Context(), `
		SELECT rule_key, entity_kind, entity_id, severity, detail, first_seen_at, last_seen_at, state
		FROM saoaf.risk_alert
		WHERE state <> 'RESOLVED'
		ORDER BY last_seen_at DESC
		LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var rule, kind, id, sev, st string
		var detail map[string]any
		var firstSeen, lastSeen time.Time
		if err := rows.Scan(&rule, &kind, &id, &sev, &detail, &firstSeen, &lastSeen, &st); err != nil {
			httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "scan error")
			return
		}
		out = append(out, map[string]any{
			"rule": rule, "entity_kind": kind, "entity_id": id,
			"severity": sev, "detail": detail, "state": st,
			"first_seen_at": firstSeen, "last_seen_at": lastSeen,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"items": out, "limit": limit, "offset": offset, "count": len(out),
	})
}

// listExitPacks returns the exit-pack risk view (I14): every ACTIVE pack
// with expiry state and completeness markers; EXPIRED packs are flagged,
// not hidden (过期数据有明确状态).
func (c Config) listExitPacks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset := parseLimit(q), parseOffset(q)
	where := "1=1"
	args := []any{limit, offset}
	arg := func(v string) string { args = append(args, v); return strconv.Itoa(len(args)) }
	if v := q.Get("vendor"); v != "" {
		where += ` AND vendor = $` + arg(v)
	}
	if v := q.Get("state"); v != "" {
		where += ` AND state = $` + arg(v)
	}
	rows, err := c.Pool.Query(r.Context(), `
		SELECT pack_key, vendor, revision, state, owner_ref,
		       substitute_provider, valid_until, digest, created_at,
		       jsonb_array_length(recovery_steps) AS steps,
		       jsonb_array_length(evidence_refs) AS evidence_count
		FROM (
			SELECT pack_key, vendor, revision, state, owner_ref,
			       substitute_provider, valid_until, digest, created_at,
			       recovery_steps, evidence_refs
			FROM saoaf.exit_pack
			WHERE `+where+`
			ORDER BY vendor, pack_key, revision DESC
		) packs
		ORDER BY vendor, pack_key, revision DESC
		LIMIT $1 OFFSET $2`, args...)
	if err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var packKey, vendor, state, owner, subst string
		var digest *string // NULL until VALIDATED — unknown stays null, never ""
		var rev int
		var steps, evidenceCount int
		var validUntil *time.Time
		var createdAt time.Time
		if err := rows.Scan(&packKey, &vendor, &rev, &state, &owner, &subst,
			&validUntil, &digest, &createdAt, &steps, &evidenceCount); err != nil {
			httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "scan error")
			return
		}
		// expiry is computed AT READ TIME — a pack crossing valid_until since
		// the last sweep shows EXPIRED_NOW, never silently healthy
		viewState := state
		if state == "ACTIVE" && validUntil != nil && validUntil.Before(time.Now()) {
			viewState = "EXPIRED"
		}
		out = append(out, map[string]any{
			"pack_key": packKey, "vendor": vendor, "revision": rev,
			"state": viewState, "declared_state": state,
			"owner_ref": owner, "substitute_provider": subst,
			"valid_until": validUntil, "digest": digest,
			"recovery_steps": steps, "evidence_count": evidenceCount,
			"created_at": createdAt,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"items": out, "limit": limit, "offset": offset, "count": len(out),
	})
}

// listDrills returns drills with state, vendor and open-finding counts
// (remediation view input). Pagination + vendor/state filters.
func (c Config) listDrills(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset := parseLimit(q), parseOffset(q)
	where := "1=1"
	args := []any{limit, offset}
	arg := func(v string) string { args = append(args, v); return strconv.Itoa(len(args)) }
	if v := q.Get("vendor"); v != "" {
		where += ` AND d.vendor = $` + arg(v)
	}
	if v := q.Get("state"); v != "" {
		where += ` AND d.state = $` + arg(v)
	}
	rows, err := c.Pool.Query(r.Context(), `
		SELECT d.drill_key, d.vendor, d.state, d.initiator, d.approver,
		       d.created_at, d.result_evidence,
		       (SELECT count(*) FROM saoaf.exit_drill_finding f
		         WHERE f.drill_key = d.drill_key AND f.state = 'OPEN') AS open_findings
		FROM saoaf.exit_drill d
		WHERE `+where+`
		ORDER BY d.created_at DESC
		LIMIT $1 OFFSET $2`, args...)
	if err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var key, vendor, state, initiator, approver, evidence string
		var open int
		var createdAt time.Time
		if err := rows.Scan(&key, &vendor, &state, &initiator, &approver, &createdAt, &evidence, &open); err != nil {
			httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "scan error")
			return
		}
		out = append(out, map[string]any{
			"drill_key": key, "vendor": vendor, "state": state,
			"initiator": initiator, "approver": approver,
			"created_at": createdAt, "result_evidence": evidence,
			"open_findings": open,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"items": out, "limit": limit, "offset": offset, "count": len(out),
	})
}

// getDrill returns one drill with its findings (remediation detail) and
// the transition log (audit trail view).
func (c Config) getDrill(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "id")
	var vendor, state, initiator, approver, evidence string
	var createdAt time.Time
	err := c.Pool.QueryRow(r.Context(), `
		SELECT vendor, state, initiator, approver, created_at, result_evidence
		FROM saoaf.exit_drill WHERE drill_key = $1`, key).
		Scan(&vendor, &state, &initiator, &approver, &createdAt, &evidence)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.WriteErr(w, r, http.StatusNotFound, "NOT_FOUND", "drill not found")
		return
	}
	if err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	findings := []map[string]any{}
	frows, ferr := c.Pool.Query(r.Context(), `
		SELECT finding_key, description, severity, state, remediation,
		       remediation_evidence, created_at, resolved_at
		FROM saoaf.exit_drill_finding
		WHERE drill_key = $1
		ORDER BY created_at`, key)
	if ferr != nil {
		// partial failure must not render as an empty success (GWT#2;
		// review R1 P3-2 — the R2 re-review caught the earlier claim
		// outrunning the code)
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	defer frows.Close()
	for frows.Next() {
		var fk, desc, sev, st, rem, remEv string
		var createdAt, resolvedAt *time.Time
		if err := frows.Scan(&fk, &desc, &sev, &st, &rem, &remEv, &createdAt, &resolvedAt); err != nil {
			httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "scan error")
			return
		}
		findings = append(findings, map[string]any{
			"finding_key": fk, "description": desc, "severity": sev,
			"state": st, "remediation": rem,
			"remediation_evidence": remEv,
			"created_at":           createdAt, "resolved_at": resolvedAt,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"drill_key": key, "vendor": vendor, "state": state,
		"initiator": initiator, "approver": approver,
		"created_at": createdAt, "result_evidence": evidence,
		"findings": findings,
	})
}

// drillLog returns the drill's transition audit trail (I15), ordered with
// the id tiebreaker.
func (c Config) drillLog(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "id")
	// consistency with getDrill: an unknown drill is 404, never an empty log
	var exists bool
	if err := c.Pool.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM saoaf.exit_drill WHERE drill_key = $1)`, key).Scan(&exists); err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	if !exists {
		httpapi.WriteErr(w, r, http.StatusNotFound, "NOT_FOUND", "drill not found")
		return
	}
	rows, err := c.Pool.Query(r.Context(), `
		SELECT from_state, to_state, actor, at
		FROM saoaf.exit_drill_transition
		WHERE drill_key = $1
		ORDER BY at, id`, key)
	if err != nil {
		httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "store error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var from, to, actor string
		var at time.Time
		if err := rows.Scan(&from, &to, &actor, &at); err != nil {
			httpapi.WriteErr(w, r, http.StatusInternalServerError, "INTERNAL", "scan error")
			return
		}
		out = append(out, map[string]any{
			"from_state": from, "to_state": to, "actor": actor, "at": at,
		})
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"items": out, "count": len(out)})
}
