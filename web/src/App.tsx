import { useEffect, useState } from 'react'

/**
 * I16: AI Resource Hub management UI (read + controlled writes via Admin
 * API only — the UI is NOT an authority). Screens: Bindings and Resource
 * Plans with search/filter; publish actions gated by server-verified
 * scopes; revision conflicts surface as explicit 409s (never silent
 * overwrite). Token/密钥/敏感正文 never enter browser storage, URL, or logs.
 */

interface WhoAmI {
  subject: string
  scopes: string[]
  canPublish: boolean
}

interface Binding {
  binding_key: string
  scope_hash: string
  priority: number
  state: string
  revision: number
}

interface Plan {
  id: string
  fingerprint: string
  status: string
  items: { requirement_id: string; provider_key: string }[]
}

// fetch with the API error envelope; never logs payloads
async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api/admin/v1${path}`, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) },
  })
  if (!res.ok) {
    const body = (await res.json().catch(() => ({}))) as { error?: { code?: string; message?: string } }
    throw Object.assign(new Error(body.error?.message ?? `HTTP ${res.status}`), {
      status: res.status,
      code: body.error?.code,
    })
  }
  return (await res.json()) as T
}

export function App() {
  const [who, setWho] = useState<WhoAmI | null>(null)
  const [view, setView] = useState<'bindings' | 'plans'>('bindings')
  const [bindings, setBindings] = useState<Binding[]>([])
  const [plans, setPlans] = useState<Plan[]>([])
  const [conflict, setConflict] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [filter, setFilter] = useState('')

  useEffect(() => {
    void api<{ subject: string; scopes: string[] }>('/whoami')
      .then((me) =>
        setWho({ subject: me.subject, scopes: me.scopes, canPublish: me.scopes.includes('resource.publish') }),
      )
      .catch(() => setWho({ subject: '', scopes: [], canPublish: false }))
  }, [])

  const loadBindings = () =>
    void api<Binding[]>('/bindings')
      .then(setBindings)
      .catch((e) => setError(e.message))
  const loadPlans = () =>
    void api<Plan[]>('/resource-plans')
      .then(setPlans)
      .catch((e) => setError(e.message))

  useEffect(() => {
    if (!who) return
    if (view === 'bindings') loadBindings()
    else loadPlans()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view, who])

  // controlled write: publish with the server-verified revision (CAS) —
  // a concurrent editor's 409 surfaces as an explicit conflict, never a
  // silent overwrite
  async function publish(key: string, revision: number) {
    setConflict(null)
    setError(null)
    try {
      await api(`/bindings/${key}/publish`, {
        method: 'POST',
        headers: { 'If-Match': String(revision) },
        body: JSON.stringify({ expected_revision: revision }),
      })
      loadBindings()
    } catch (e) {
      if ((e as { status?: number }).status === 409) {
        setConflict(`并发冲突：${key} 的 revision 已被其他操作修改，操作被拒绝（未覆盖）。`)
        loadBindings() // refresh to the authoritative state (GWT#5)
      } else {
        setError(e instanceof Error ? e.message : String(e))
      }
    }
  }

  const filtered = bindings.filter(
    (b) => !filter || b.binding_key.includes(filter) || b.state.toLowerCase().includes(filter.toLowerCase()),
  )

  return (
    <main style={{ fontFamily: 'system-ui, sans-serif', padding: '2rem', maxWidth: 960 }}>
      <h1>AI Resource Hub</h1>
      {who && (
        <p data-testid="who">
          {who.subject ? `操作者：${who.subject}` : '未认证'} · 权限：
          {who.canPublish ? '发布' : '只读'}
        </p>
      )}
      <nav style={{ marginBottom: '1rem' }}>
        <button onClick={() => setView('bindings')} disabled={view === 'bindings'}>
          Bindings
        </button>{' '}
        <button onClick={() => setView('plans')} disabled={view === 'plans'}>
          Resource Plans
        </button>
      </nav>

      {conflict && (
        <p role="alert" data-testid="conflict" style={{ color: '#b00', border: '1px solid #b00', padding: '0.5rem' }}>
          {conflict}
        </p>
      )}
      {error && (
        <p role="alert" data-testid="error" style={{ color: '#900' }}>
          {error}
        </p>
      )}

      {view === 'bindings' && (
        <section>
          <input
            placeholder="搜索 binding / 状态"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            aria-label="搜索"
          />
          <table style={{ width: '100%', borderCollapse: 'collapse', marginTop: '0.5rem' }}>
            <thead>
              <tr>
                <th style={{ textAlign: 'left' }}>Binding</th>
                <th style={{ textAlign: 'left' }}>State</th>
                <th style={{ textAlign: 'left' }}>Priority</th>
                <th style={{ textAlign: 'left' }}>Revision</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {filtered.map((b) => (
                <tr key={b.binding_key} data-testid="binding-row">
                  <td>{b.binding_key}</td>
                  <td>{b.state}</td>
                  <td>{b.priority}</td>
                  <td data-testid={`rev-${b.binding_key}`}>{b.revision}</td>
                  <td>
                    {/* 无权用户看不到写入口（服务端 403 是最终防线，UI
                        隐藏只是体验——E2E 同时断言两侧） */}
                    {who?.canPublish && (
                      <button onClick={() => void publish(b.binding_key, b.revision)}>
                        发布
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}

      {view === 'plans' && (
        <section>
          <table style={{ width: '100%', borderCollapse: 'collapse' }}>
            <thead>
              <tr>
                <th style={{ textAlign: 'left' }}>Plan</th>
                <th style={{ textAlign: 'left' }}>Status</th>
                <th style={{ textAlign: 'left' }}>Items</th>
              </tr>
            </thead>
            <tbody>
              {plans.map((p) => (
                <tr key={p.id} data-testid="plan-row">
                  <td>{p.id}</td>
                  <td>{p.status}</td>
                  <td>{p.items.map((i) => `${i.requirement_id}→${i.provider_key}`).join(', ')}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}
    </main>
  )
}
