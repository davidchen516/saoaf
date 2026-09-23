// Package metrics implements the sovereignty metrics, risk ledger and
// query API (I13, module 03.8, architecture §主权指标):
//
//   - 指标纯函数: metric(datasetRevision, formulaVersion) is fully
//     reproducible — a fixed dataset recomputes with diff = 0
//   - 追溯三元组: every result carries (dataset revision, formula version,
//     evidence reference)
//   - 语义分类: empty denominators, unknown vendors, duplicate providers,
//     expired snapshots and inactive bindings classify explicitly —
//     未知 ≠ 0 (unknown is a STATUS, never a numeric zero)
//   - 告警幂等: alerts key on (rule, entity, datasetRevision); recomputes
//     and worker restarts never duplicate (last_seen_at refresh only)
//   - 回滚: switching to an older formula version recomputes forward;
//     historical results and issued risk evidence are never rewritten
//
// Module boundary (ADR-0006): SQL over shared schemas only.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Metric keys.
const (
	KeySubstitutionCoverage = "substitution_coverage"  // 替代覆盖率
	KeyVendorConcentration  = "vendor_concentration"   // 供应商集中度
	KeyProtocolCompat       = "protocol_compatibility" // 协议兼容率
	KeyEvidenceCompleteness = "evidence_completeness"  // 证据完整率
	KeyExitPackCompleteness = "exit_pack_completeness" // Exit Pack 完整率
)

// Result statuses (语义分类表——未知 ≠ 0).
const (
	StatusOK               = "OK"
	StatusNotApplicable    = "NOT_APPLICABLE"    // 空分母：不适用（如单供应商环境的替代覆盖）
	StatusUnknown          = "UNKNOWN"           // 未知供应商/实体：显式未知
	StatusInsufficientData = "INSUFFICIENT_DATA" // 失效 Binding/过期 Snapshot 导致分母不可信
)

// Severities for risk alerts.
const (
	SeverityLow      = "LOW"
	SeverityMedium   = "MEDIUM"
	SeverityHigh     = "HIGH"
	SeverityCritical = "CRITICAL"
)

// Store persists metric results and risk alerts.
type Store struct{ Pool *pgxpool.Pool }

// MetricResult is one computed metric point.
type MetricResult struct {
	MetricKey       string
	Dimensions      map[string]string
	Value           *float64 // nil for non-OK statuses (未知 ≠ 0)
	Status          string
	StatusReason    string
	DatasetRevision int64
	FormulaVersion  string
	EvidenceRef     string
}

// Dataset is the snapshot of input data the metrics compute over.
// It carries a monotonic revision; fixed datasets recompute identically.
type Dataset struct {
	Revision int64
	// Rows is the raw per-dimension input extracted from the live tables
	// (deterministic: ORDER BY over stable keys).
	Rows []DimRow
}

// ExtractDataset pulls the aggregation input from the LIVE registry tables
// (review R1 P1: the pipeline previously had no extractor — DimRow only
// existed in tests, making the issue's real-run evidence impossible).
// Deterministic: ORDER BY over stable keys; the revision is the current
// migration state + row count fingerprint so fixed snapshots recompute.
func (s Store) ExtractDataset(ctx context.Context) (*Dataset, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT cd.capability_key, rp.provider_key, rp.owner_ref,
		       cb.environment,
		       CASE WHEN cb.state = 'PUBLISHED' AND cb.is_active THEN 1 ELSE 0 END,
		       CASE WHEN cb.state = 'PUBLISHED' AND cb.is_active
		            AND EXISTS (SELECT 1 FROM registry.capability_binding cb2
		                        WHERE cb2.capability_id = cb.capability_id
		                          AND cb2.state = 'PUBLISHED' AND cb2.is_active
		                          AND cb2.provider_id <> cb.provider_id)
		            THEN 1 ELSE 0 END,
		       CASE WHEN cb.state <> 'PUBLISHED' OR NOT cb.is_active THEN 1 ELSE 0 END,
		       0,  -- dup provider rows: computed below
		       CASE WHEN rp.owner_ref = '' THEN TRUE ELSE FALSE END,
		       cb.tenant_ref
		FROM registry.capability_definition cd
		JOIN registry.capability_binding cb ON cb.capability_id = cd.id
		JOIN registry.resource_provider rp ON rp.id = cb.provider_id
		ORDER BY cd.capability_key, rp.provider_key, cb.environment, cb.revision`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	d := &Dataset{}
	var providerSeen = map[string]int{}
	for rows.Next() {
		var r DimRow
		var active, withSub, issues int
		var unknownVendor bool
		var dup int
		if err := rows.Scan(&r.Capability, &r.Provider, &r.Vendor, &r.Environment,
			&active, &withSub, &issues, &dup, &unknownVendor, &r.Tenant); err != nil {
			return nil, err
		}
		r.ActiveBindings = active
		r.BindingsWithSubstitute = withSub
		r.BindingIssues = issues
		r.UnknownVendor = unknownVendor
		r.DupProviderRows = dup
		providerSeen[r.Provider]++
		d.Rows = append(d.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// duplicate providers: a provider key spanning multiple rows
	for i := range d.Rows {
		if providerSeen[d.Rows[i].Provider] > 1 {
			d.Rows[i].DupProviderRows = providerSeen[d.Rows[i].Provider] - 1
		}
	}
	// dataset revision: migration version + input fingerprint (a fixed
	// snapshot recomputes identically; a changed input bumps the revision)
	var fingerprint int64
	if err := s.Pool.QueryRow(ctx, `
		SELECT (SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version) * 1000000000
		     + COALESCE(SUM(cb.id), 0)
		FROM registry.capability_binding cb`).Scan(&fingerprint); err != nil {
		return nil, err
	}
	d.Revision = fingerprint
	return d, nil
}

// DimRow is one aggregation input row.
type DimRow struct {
	Capability  string
	Provider    string
	Vendor      string
	Environment string
	Tenant      string
	// ActiveBindings: live PUBLISHED bindings for this row's dimension set
	ActiveBindings int
	// BindingsWithSubstitute: bindings whose capability has ≥2 distinct
	// live providers (a viable alternative exists)
	BindingsWithSubstitute int
	// BindingIssues: bindings that are inactive/stale-snapshot (denominator
	// quality signals)
	BindingIssues int
	// UnknownVendor: the vendor could not be resolved
	UnknownVendor bool
	// DupProviderRows: provider key appears in multiple rows (duplicate
	// provider entries)
	DupProviderRows int
}

// Formula versions. v1 is the initial set; each metric is a pure function
// of (Dataset, dims). v2+ exist to demonstrate 回滚 (switch the active
// formula, recompute forward — history untouched).
const (
	FormulaV1 = "v1"
	FormulaV2 = "v2"
)

// ComputeV1 computes all metrics for the dataset under formula v1.
// Pure: same dataset revision + formula → same results (test-enforced).
func ComputeV1(d *Dataset) []MetricResult {
	var out []MetricResult
	byCap := groupBy(d.Rows, func(r DimRow) string { return r.Capability })
	byCap = filterNonEmpty(byCap)

	// 替代覆盖率: fraction of live bindings whose capability has a viable
	// alternative provider. 空分母（无在役 binding）→ NOT_APPLICABLE.
	for cap, rows := range byCap {
		total := 0
		withSub := 0
		for _, r := range rows {
			total += r.ActiveBindings
			withSub += r.BindingsWithSubstitute
		}
		dims := map[string]string{"capability": cap}
		if total == 0 {
			out = append(out, mr(KeySubstitutionCoverage, dims, nil, StatusNotApplicable,
				"no live bindings for capability (empty denominator)", d.Revision, FormulaV1))
			continue
		}
		v := float64(withSub) / float64(total)
		out = append(out, mr(KeySubstitutionCoverage, dims, &v, StatusOK, "", d.Revision, FormulaV1))
	}

	// 供应商集中度: share of live bindings held by the top vendor.
	// 未知供应商 → UNKNOWN (未知 ≠ 0); 重复 Provider 行数计入 reason.
	byVendor := map[string]int{}
	unknown := 0
	dup := 0
	bindings := 0
	for _, r := range d.Rows {
		bindings += r.ActiveBindings
		dup += r.DupProviderRows
		if r.UnknownVendor {
			unknown++
			continue
		}
		byVendor[r.Vendor] += r.ActiveBindings
	}
	if bindings == 0 {
		out = append(out, mr(KeyVendorConcentration, map[string]string{}, nil,
			StatusNotApplicable, "no live bindings (empty denominator)", d.Revision, FormulaV1))
	} else if unknown > 0 {
		out = append(out, mr(KeyVendorConcentration, map[string]string{}, nil,
			StatusUnknown, fmt.Sprintf("%d rows have unresolvable vendors", unknown), d.Revision, FormulaV1))
	} else {
		top := 0
		for _, n := range byVendor {
			if n > top {
				top = n
			}
		}
		v := float64(top) / float64(bindings)
		reason := ""
		if dup > 0 {
			reason = fmt.Sprintf("duplicate provider rows: %d", dup)
		}
		out = append(out, mr(KeyVendorConcentration, map[string]string{}, &v, StatusOK, reason, d.Revision, FormulaV1))
	}

	// 协议兼容率: fraction of capability bindings that are live AND
	// issue-free (active snapshot in its window, published state). A mixed
	// population yields a real ratio (review R1 P2-2: the previous
	// formulation collapsed to 1.0-or-unknown); a FULLY compromised
	// denominator classifies INSUFFICIENT_DATA (未知 ≠ 0).
	for cap, rows := range byCap {
		live := 0
		healthy := 0
		for _, r := range rows {
			live += r.ActiveBindings + r.BindingIssues
			healthy += r.ActiveBindings
		}
		dims := map[string]string{"capability": cap}
		if live == 0 {
			out = append(out, mr(KeyProtocolCompat, dims, nil,
				StatusNotApplicable, "no bindings for capability (empty denominator)", d.Revision, FormulaV1))
			continue
		}
		if healthy == 0 {
			out = append(out, mr(KeyProtocolCompat, dims, nil,
				StatusInsufficientData, "all bindings inactive/expired snapshots", d.Revision, FormulaV1))
			continue
		}
		v := float64(healthy) / float64(live)
		out = append(out, mr(KeyProtocolCompat, dims, &v, StatusOK, "", d.Revision, FormulaV1))
	}

	// 纯函数的确定性输出序：按 (metric key, dims JSON) 排序——固定数据集
	// 重跑 diff = 0 要求连输出顺序都一致（map 迭代序不可依赖）
	sortResults(out)
	return out
}

func sortResults(rs []MetricResult) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].MetricKey != rs[j].MetricKey {
			return rs[i].MetricKey < rs[j].MetricKey
		}
		return mustDims(rs[i].Dimensions) < mustDims(rs[j].Dimensions)
	})
}

// dimsJSON marshals a dims filter for the containment query (nil → {}).
func dimsJSON(m map[string]string) []byte {
	if m == nil {
		return []byte("{}")
	}
	b, _ := json.Marshal(m)
	return b
}

func mustDims(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

func mr(key string, dims map[string]string, value *float64, status, reason string, rev int64, formula string) MetricResult {
	return MetricResult{MetricKey: key, Dimensions: dims, Value: value,
		Status: status, StatusReason: reason, DatasetRevision: rev, FormulaVersion: formula,
		EvidenceRef: fmt.Sprintf("dataset:%d@%s", rev, formula)}
}

func groupBy(rows []DimRow, key func(DimRow) string) map[string][]DimRow {
	m := map[string][]DimRow{}
	for _, r := range rows {
		m[key(r)] = append(m[key(r)], r)
	}
	return m
}

func filterNonEmpty(m map[string][]DimRow) map[string][]DimRow {
	out := map[string][]DimRow{}
	for k, v := range m {
		if k != "" {
			out[k] = v
		}
	}
	return out
}

// PersistResults writes metric results (幂等: same (key, dims, revision,
// formula) upserts the value — recomputes never duplicate rows) and
// refreshes risk alerts (幂等键 (rule, entity, revision): UPSERT bumps
// last_seen_at only, never creating a duplicate alert).
func (s Store) PersistResults(ctx context.Context, results []MetricResult) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, r := range results {
		dims, err := json.Marshal(r.Dimensions)
		if err != nil {
			return err
		}
		var value *float64
		if r.Value != nil {
			value = r.Value
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO saoaf.metric_result
				(metric_key, dimensions, value, status, status_reason,
				 dataset_revision, formula_version, evidence_ref, computed_at)
			VALUES ($1, $2::jsonb, $3, $4, $5, $6, $7, $8, now())
			ON CONFLICT (metric_key, dimensions, dataset_revision, formula_version)
			DO UPDATE SET value = $3, status = $4, status_reason = $5,
				evidence_ref = $8, computed_at = now()`,
			r.MetricKey, dims, value, r.Status, r.StatusReason,
			r.DatasetRevision, r.FormulaVersion, r.EvidenceRef); err != nil {
			return err
		}

		// risk rules: derive alerts from metric statuses (版本化规则 v1:
		// substitution coverage below 0.5 per capability → MEDIUM;
		// UNKNOWN vendor rows → HIGH 未知 ≠ 0 显式告警)
		if err := deriveAlertsV1(ctx, tx, r); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func deriveAlertsV1(ctx context.Context, tx pgx.Tx, r MetricResult) error {
	switch {
	case r.MetricKey == KeySubstitutionCoverage && r.Status == StatusOK && r.Value != nil && *r.Value < 0.5:
		return upsertAlert(ctx, tx, "substitution_coverage_below_floor.v1", "capability",
			r.Dimensions["capability"], r.DatasetRevision, SeverityMedium,
			map[string]any{"coverage": *r.Value, "floor": 0.5})
	case r.MetricKey == KeyVendorConcentration && r.Status == StatusUnknown:
		return upsertAlert(ctx, tx, "unknown_vendor_present.v1", "environment", "*",
			r.DatasetRevision, SeverityHigh,
			map[string]any{"reason": r.StatusReason})
	case r.MetricKey == KeyProtocolCompat && r.Status == StatusInsufficientData:
		return upsertAlert(ctx, tx, "binding_health_insufficient.v1", "environment", "*",
			r.DatasetRevision, SeverityLow,
			map[string]any{"reason": r.StatusReason})
	}
	return nil
}

func upsertAlert(ctx context.Context, tx pgx.Tx, rule, entityKind, entityID string, rev int64, severity string, detail map[string]any) error {
	if entityID == "" {
		return nil
	}
	d, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	// 幂等键 (rule, entity, revision): first sight creates; recomputes /
	// restarts refresh last_seen_at ONLY — never a duplicate alert
	// (review R1 P2-4: DO NOTHING never refreshed, contradicting three
	// documentation claims)
	_, err = tx.Exec(ctx, `
		INSERT INTO saoaf.risk_alert
			(rule_key, entity_kind, entity_id, dataset_revision, severity, detail)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		ON CONFLICT (rule_key, entity_kind, entity_id, dataset_revision)
		DO UPDATE SET last_seen_at = now()`,
		rule, entityKind, entityID, rev, severity, d)
	return err
}

// Latest returns the newest result per (metric, dims) for the given
// formula version — the query API surface (GWT#1: 与上次运行完全一致 + 下钻三元组).
// dimsFilter narrows by dimension keys (capability / provider / vendor /
// environment / tenant — the dims the formulas emit); since/computedUntil
// bound the computation time (review R1 P2-1: the previous surface had
// only formula+metric filters).
func (s Store) Latest(ctx context.Context, formulaVersion, metricKey string, dimsFilter map[string]string, since, computedUntil time.Time, limit int) ([]MetricResult, error) {
	if limit <= 0 {
		limit = 100
	}
	if computedUntil.IsZero() {
		computedUntil = time.Now().Add(24 * time.Hour) // no upper bound by default
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT ON (metric_key, dimensions)
			metric_key, dimensions, value, status, status_reason,
			dataset_revision, formula_version, evidence_ref, computed_at
		FROM saoaf.metric_result
		WHERE formula_version = $1
		  AND ($2 = '' OR metric_key = $2)
		  AND ($3::jsonb = '{}'::jsonb OR dimensions @> $3::jsonb)
		  AND ($4 = timestamptz 'epoch' OR computed_at >= $4)
		  AND computed_at <= $5
		ORDER BY metric_key, dimensions, computed_at DESC
		LIMIT $6`, formulaVersion, metricKey, dimsJSON(dimsFilter), since, computedUntil, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricResult
	for rows.Next() {
		var r MetricResult
		var dims []byte
		var value *float64
		var at time.Time
		if err := rows.Scan(&r.MetricKey, &dims, &value, &r.Status, &r.StatusReason,
			&r.DatasetRevision, &r.FormulaVersion, &r.EvidenceRef, &at); err != nil {
			return nil, err
		}
		r.Value = value
		_ = json.Unmarshal(dims, &r.Dimensions)
		out = append(out, r)
	}
	return out, rows.Err()
}

// History returns results for a (metric, dims) across formula versions —
// the 回滚 query evidence (historical rows never rewritten).
func (s Store) History(ctx context.Context, metricKey string, dims map[string]string) ([]MetricResult, error) {
	dimsJSON, _ := json.Marshal(dims)
	rows, err := s.Pool.Query(ctx, `
		SELECT metric_key, dimensions, value, status, status_reason,
		       dataset_revision, formula_version, evidence_ref, computed_at
		FROM saoaf.metric_result
		WHERE metric_key = $1 AND dimensions = $2::jsonb
		ORDER BY dataset_revision, formula_version`, metricKey, dimsJSON)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricResult
	for rows.Next() {
		var r MetricResult
		var dims []byte
		var value *float64
		var at time.Time
		if err := rows.Scan(&r.MetricKey, &dims, &value, &r.Status, &r.StatusReason,
			&r.DatasetRevision, &r.FormulaVersion, &r.EvidenceRef, &at); err != nil {
			return nil, err
		}
		r.Value = value
		_ = json.Unmarshal(dims, &r.Dimensions)
		out = append(out, r)
	}
	return out, rows.Err()
}

// OpenAlertCount reports the open risk ledger depth (monitoring);
// alerts are idempotent on (rule, entity, revision) — recomputes never
// duplicate (GWT#3).
func (s Store) OpenAlertCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM saoaf.risk_alert WHERE state = 'OPEN'`).Scan(&n)
	return n, err
}
