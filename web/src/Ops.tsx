import { useEffect, useState } from 'react'

/**
 * I17: Sovereignty Operations views — a READ-ONLY operations console over
 * the ops API (/api/admin/v1/ops/*). The UI is NOT an authority and not a
 * control: drill operations stay on their API (illegal operations are
 * rejected server-side by the I15 state machine); this surface only
 * renders authoritative state. Server-side pagination everywhere (the
 * browser never loads full tables); unknown/missing/expired data renders
 * as EXPLICIT states — never as zero risk.
 */

interface Paged<T> {
  items: T[]
  limit: number
  offset: number
  count: number
}

interface MetricRow {
  metric_key: string
  dimensions: Record<string, string>
  value: number | null
  status: string
  status_reason: string
  dataset_revision: number
  formula_version: string
  evidence_ref: string
  computed_at: string
  broken_reason?: string
}

interface AlertRow {
  rule: string
  entity_kind: string
  entity_id: string
  severity: string
  state: string
  first_seen_at: string
  last_seen_at: string
}

interface PackRow {
  pack_key: string
  vendor: string
  revision: number
  state: string
  declared_state: string
  valid_until: string | null
  digest: string | null
  recovery_steps: number
  evidence_count: number
}

interface DrillRow {
  drill_key: string
  vendor: string
  state: string
  initiator: string
  approver: string
  created_at: string
  result_evidence: string
  open_findings: number
}

interface Finding {
  finding_key: string
  description: string
  severity: string
  state: string
  remediation: string
}

interface DrillDetail extends DrillRow {
  findings: Finding[]
}

interface Overview {
  open_alerts: number
  expired_packs: number
  active_drills: number
  open_findings: number
  quarantine_depth: number
  unknown_metrics: number
  unknown_fields: string[]
  generated_at: string
}

// fetch with the FROZEN error envelope (flat error_code/message/request_id)
async function opsApi<T>(path: string): Promise<T> {
  const res = await fetch(`/api/admin/v1${path}`)
  if (!res.ok) {
    const body = (await res.json().catch(() => ({}))) as {
      error_code?: string
      message?: string
      request_id?: string
    }
    throw Object.assign(new Error(body.message ?? `HTTP ${res.status}`), {
      status: res.status,
      code: body.error_code,
      requestId: body.request_id,
    })
  }
  return (await res.json()) as T
}

// 未知 ≠ 0: the status badge carries the semantics; a null value never
// renders as a number
function ValueCell({ row }: { row: MetricRow }) {
  if (row.value !== null) {
    return <span data-testid="metric-value">{row.value.toFixed(2)}</span>
  }
  const label =
    row.status === 'UNKNOWN' ? '未知' : row.status === 'NOT_APPLICABLE' ? '不适用' : '数据不足'
  return (
    <span
      data-testid="metric-unknown"
      style={{ color: '#950', fontWeight: 600 }}
      title={row.status_reason}
    >
      {label}
    </span>
  )
}

// an overview field whose query failed renders unknown — never 0
function UnknownBadge() {
  return (
    <span data-testid="overview-unknown" style={{ color: '#950', fontWeight: 600 }}>
      未知
    </span>
  )
}

function StatusBadge({ status }: { status: string }) {
  const color =
    status === 'OK'
      ? '#070'
      : status === 'UNKNOWN' || status === 'INSUFFICIENT_DATA'
        ? '#950'
        : '#b00'
  return (
    <span style={{ color, border: `1px solid ${color}`, padding: '0 0.35rem', fontSize: 12 }}>
      {status}
    </span>
  )
}

type OpsView = 'overview' | 'metrics' | 'broken' | 'alerts' | 'packs' | 'drills'

export function Ops() {
  const [view, setView] = useState<OpsView>('overview')
  const [error, setError] = useState<string | null>(null)
  const [page, setPage] = useState(0) // server-side pagination cursor
  const limit = 50
  const [overview, setOverview] = useState<Overview | null>(null)
  const [metrics, setMetrics] = useState<Paged<MetricRow> | null>(null)
  const [alerts, setAlerts] = useState<Paged<AlertRow> | null>(null)
  const [packs, setPacks] = useState<Paged<PackRow> | null>(null)
  const [drills, setDrills] = useState<Paged<DrillRow> | null>(null)
  const [drillDetail, setDrillDetail] = useState<DrillDetail | null>(null)
  const [drillLog, setDrillLog] = useState<{ items: { from_state: string; to_state: string; actor: string; at: string }[] } | null>(null)
  const [selectedDrill, setSelectedDrill] = useState<string | null>(null)

  useEffect(() => {
    setError(null)
    const offset = page * limit
    const load = (p: Promise<unknown>) => p.catch((e) => setError(e instanceof Error ? e.message : String(e)))
    if (view === 'overview') {
      setOverview(null)
      load(opsApi<Overview>('/ops/overview').then(setOverview))
    } else if (view === 'metrics') {
      setMetrics(null)
      load(opsApi<Paged<MetricRow>>(`/ops/metrics?limit=${limit}&offset=${offset}`).then(setMetrics))
    } else if (view === 'broken') {
      setMetrics(null)
      load(opsApi<Paged<MetricRow>>(`/ops/metrics/broken?limit=${limit}&offset=${offset}`).then(setMetrics))
    } else if (view === 'alerts') {
      setAlerts(null)
      load(opsApi<Paged<AlertRow>>(`/ops/alerts?limit=${limit}&offset=${offset}`).then(setAlerts))
    } else if (view === 'packs') {
      setPacks(null)
      load(opsApi<Paged<PackRow>>(`/ops/exit-packs?limit=${limit}&offset=${offset}`).then(setPacks))
    } else if (view === 'drills') {
      setDrills(null)
      load(opsApi<Paged<DrillRow>>(`/ops/drills?limit=${limit}&offset=${offset}`).then(setDrills))
    }
  }, [view, page])

  async function openDrill(key: string) {
    setSelectedDrill(key)
    setDrillDetail(null)
    setDrillLog(null)
    try {
      const d = await opsApi<DrillDetail>(`/ops/drills/${key}`)
      setDrillDetail(d)
      setDrillLog(await opsApi<{ items: typeof drillLog extends null ? never : { from_state: string; to_state: string; actor: string; at: string }[] }>(`/ops/drills/${key}/log`))
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  const navBtn = (v: OpsView, label: string) => (
    <button onClick={() => { setView(v); setPage(0); setSelectedDrill(null) }} disabled={view === v}>
      {label}
    </button>
  )

  const pager = (data: Paged<unknown> | null) =>
    data && (
      <div style={{ margin: '0.5rem 0' }}>
        <button onClick={() => setPage(Math.max(0, page - 1))} disabled={page === 0}>
          上一页
        </button>{' '}
        <span data-testid="page-info">
          第 {page + 1} 页 · 本页 {data.count} 条（服务端分页 limit={data.limit}）
        </span>{' '}
        <button onClick={() => setPage(page + 1)} disabled={data.count < data.limit}>
          下一页
        </button>
      </div>
    )

  return (
    <section style={{ marginTop: '1.5rem' }}>
      <h2>Sovereignty Operations</h2>
      <nav style={{ marginBottom: '1rem' }}>
        {navBtn('overview', '总览')}
        {navBtn('metrics', '主权指标')}
        {navBtn('broken', '证据断链')}
        {navBtn('alerts', '风险')}
        {navBtn('packs', 'Exit Pack')}
        {navBtn('drills', 'Drill / 整改')}
      </nav>

      {error && (
        <p role="alert" data-testid="ops-error" style={{ color: '#900' }}>
          {error}
        </p>
      )}

      {view === 'overview' &&
        (overview ? (
          <div data-testid="ops-overview">
            <p>
              未决风险告警 <strong data-testid="open-alerts">{overview.open_alerts}</strong> · 过期 Exit Pack{' '}
              <strong data-testid="expired-packs">{overview.expired_packs}</strong> · 进行中 Drill{' '}
              <strong>{overview.active_drills}</strong> · 未闭环整改 <strong>{overview.open_findings}</strong>
            </p>
            <p>
              隔离区待处置{' '}
              {overview.unknown_fields.includes('quarantine_depth') ? (
                <UnknownBadge />
              ) : (
                <strong>{overview.quarantine_depth}</strong>
              )}{' '}
              · 显式未知指标{' '}
              {overview.unknown_fields.includes('unknown_metrics') ? (
                <UnknownBadge />
              ) : (
                <strong data-testid="unknown-metrics">{overview.unknown_metrics}</strong>
              )}
              {overview.unknown_fields.length > 0 && (
                <em>（不可用字段：{overview.unknown_fields.join(', ')}）</em>
              )}
            </p>
            <p style={{ color: '#666', fontSize: 12 }}>生成于 {overview.generated_at}</p>
          </div>
        ) : (
          <p>加载中…</p>
        ))}

      {view === 'metrics' && metrics && (
        <>
          {pager(metrics)}
          <table style={{ width: '100%', borderCollapse: 'collapse' }} data-testid="ops-metrics">
            <thead>
              <tr>
                <th style={{ textAlign: 'left' }}>指标</th>
                <th style={{ textAlign: 'left' }}>维度</th>
                <th style={{ textAlign: 'left' }}>值</th>
                <th style={{ textAlign: 'left' }}>状态</th>
                <th style={{ textAlign: 'left' }}>下钻三元组</th>
              </tr>
            </thead>
            <tbody>
              {metrics.items.map((m, i) => (
                <tr key={i} data-testid="metric-row">
                  <td>{m.metric_key}</td>
                  <td style={{ fontSize: 12 }}>
                    {Object.entries(m.dimensions).map(([k, v]) => `${k}=${v}`).join(' · ')}
                  </td>
                  <td>
                    <ValueCell row={m} />
                  </td>
                  <td>
                    <StatusBadge status={m.status} />
                    {m.status_reason && (
                      <span style={{ fontSize: 11, color: '#666' }}> {m.status_reason}</span>
                    )}
                  </td>
                  <td style={{ fontSize: 11 }} data-testid="drill-triple">
                    {m.formula_version} / rev {m.dataset_revision} /{' '}
                    {m.evidence_ref || <em style={{ color: '#950' }}>（无 Evidence 引用）</em>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}

      {view === 'broken' && metrics && (
        <>
          <p style={{ color: '#950' }}>以下指标的 Evidence 链断裂（引用缺失或无法解析）——不计为健康：</p>
          {pager(metrics)}
          <table style={{ width: '100%', borderCollapse: 'collapse' }} data-testid="ops-broken">
            <thead>
              <tr>
                <th style={{ textAlign: 'left' }}>指标</th>
                <th style={{ textAlign: 'left' }}>断链原因</th>
                <th style={{ textAlign: 'left' }}>证据引用</th>
              </tr>
            </thead>
            <tbody>
              {metrics.items.map((m, i) => (
                <tr key={i}>
                  <td>{m.metric_key}</td>
                  <td style={{ color: '#950' }}>{m.broken_reason}</td>
                  <td style={{ fontSize: 11 }}>{m.evidence_ref || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}

      {view === 'alerts' && alerts && (
        <>
          {pager(alerts)}
          <table style={{ width: '100%', borderCollapse: 'collapse' }} data-testid="ops-alerts">
            <thead>
              <tr>
                <th style={{ textAlign: 'left' }}>规则</th>
                <th style={{ textAlign: 'left' }}>实体</th>
                <th style={{ textAlign: 'left' }}>严重度</th>
                <th style={{ textAlign: 'left' }}>状态</th>
              </tr>
            </thead>
            <tbody>
              {alerts.items.map((a, i) => (
                <tr key={i}>
                  <td>{a.rule}</td>
                  <td>
                    {a.entity_kind}/{a.entity_id}
                  </td>
                  <td style={{ color: a.severity === 'CRITICAL' || a.severity === 'HIGH' ? '#b00' : '#950' }}>
                    {a.severity}
                  </td>
                  <td>{a.state}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}

      {view === 'packs' && packs && (
        <>
          {pager(packs)}
          <table style={{ width: '100%', borderCollapse: 'collapse' }} data-testid="ops-packs">
            <thead>
              <tr>
                <th style={{ textAlign: 'left' }}>Pack</th>
                <th style={{ textAlign: 'left' }}>状态</th>
                <th style={{ textAlign: 'left' }}>有效期至</th>
                <th style={{ textAlign: 'left' }}>完备性</th>
              </tr>
            </thead>
            <tbody>
              {packs.items.map((p, i) => (
                <tr key={i}>
                  <td>
                    {p.pack_key} <span style={{ color: '#666' }}>rev {p.revision}</span>
                  </td>
                  <td>
                    {p.state === 'EXPIRED' ? (
                      <span style={{ color: '#b00', fontWeight: 600 }}>已过期（读取时判定）</span>
                    ) : (
                      p.state
                    )}
                  </td>
                  <td>{p.valid_until ?? '永久'}</td>
                  <td>
                    恢复步骤 {p.recovery_steps} · 证据 {p.evidence_count}
                    {p.digest === null && <em style={{ color: '#950' }}>（digest 未生成）</em>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}

      {view === 'drills' && (
        <>
          {drills && pager(drills)}
          {drills && (
            <table style={{ width: '100%', borderCollapse: 'collapse' }} data-testid="ops-drills">
              <thead>
                <tr>
                  <th style={{ textAlign: 'left' }}>Drill</th>
                  <th style={{ textAlign: 'left' }}>状态</th>
                  <th style={{ textAlign: 'left' }}>发起/审批</th>
                  <th style={{ textAlign: 'left' }}>未闭环</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {drills.items.map((d) => (
                  <tr key={d.drill_key}>
                    <td>
                      {d.drill_key} <span style={{ color: '#666' }}>{d.vendor}</span>
                    </td>
                    <td>{d.state}</td>
                    <td style={{ fontSize: 12 }}>
                      {d.initiator} / {d.approver || '—'}
                    </td>
                    <td>{d.open_findings}</td>
                    <td>
                      <button onClick={() => void openDrill(d.drill_key)}>详情</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {selectedDrill && drillDetail && (
            <div style={{ marginTop: '1rem', border: '1px solid #ccc', padding: '1rem' }} data-testid="drill-detail">
              <h3>
                {drillDetail.drill_key} · {drillDetail.state}
              </h3>
              <p style={{ fontSize: 12 }}>
                结果证据：{drillDetail.result_evidence || <em style={{ color: '#950' }}>（未提交）</em>}
              </p>
              <h4>整改发现（findings）</h4>
              <table style={{ width: '100%', borderCollapse: 'collapse' }}>
                <thead>
                  <tr>
                    <th style={{ textAlign: 'left' }}>发现</th>
                    <th style={{ textAlign: 'left' }}>严重度</th>
                    <th style={{ textAlign: 'left' }}>状态</th>
                    <th style={{ textAlign: 'left' }}>整改</th>
                  </tr>
                </thead>
                <tbody>
                  {drillDetail.findings.map((f) => (
                    <tr key={f.finding_key}>
                      <td>{f.description}</td>
                      <td>{f.severity}</td>
                      <td>{f.state}</td>
                      <td>{f.remediation || <em>待整改</em>}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {drillLog && (
                <>
                  <h4>状态机审计轨迹</h4>
                  <ol style={{ fontSize: 12 }}>
                    {drillLog.items.map((t, i) => (
                      <li key={i}>
                        {t.from_state} → {t.to_state}（{t.actor}）
                      </li>
                    ))}
                  </ol>
                </>
              )}
              <p style={{ fontSize: 11, color: '#666' }}>
                Drill 状态变更由 API 的状态机裁决（I15）；本界面不提供绕过路径。
              </p>
            </div>
          )}
        </>
      )}
    </section>
  )
}
