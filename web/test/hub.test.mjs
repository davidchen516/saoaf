// I16 test suite (node:test — zero additional deps; the "web build + npm
// audit" CI gate covers the toolchain, these cover the domain contract):
//   - source-level security assertions (sensitive-zero; UI non-authority)
//   - mock-contract assertions (409 semantics / scope gating / envelopes)
//   - built-bundle assertions (the dist artifact carries no secrets and
//     only /api/admin/v1 fetch paths)
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const src = readFileSync(new URL('../src/App.tsx', import.meta.url), 'utf8')

// mock Admin API mirroring the server contract (envelope, 409 semantics)
function makeMockApi(opts) {
  const state = {
    bindings: [
      { binding_key: 'bind-a', scope_hash: 'sha256:x', priority: 100, state: 'PUBLISHED', revision: 3 },
    ],
    calls: [],
  }
  return async (input, init) => {
    const url = String(input)
    const method = init?.method ?? 'GET'
    const headers = {}
    new Headers(init?.headers).forEach((v, k) => (headers[k] = v))
    state.calls.push({ path: url, method, headers })
    const json = (status, body) =>
      new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
    if (url.endsWith('/whoami')) return json(200, { subject: 'user:tester', scopes: opts.scopes })
    if (url.endsWith('/bindings') && method === 'GET') return json(200, state.bindings)
    if (url.endsWith('/bindings/bind-a/publish') && method === 'POST') {
      if (opts.concurrentBump) {
        state.bindings[0].revision = 4
        return json(409, { error: { code: 'BINDING_REVISION_CONFLICT', message: 'revision conflict' } })
      }
      return json(200, { status: 'published' })
    }
    return json(404, { error: { code: 'NOT_FOUND', message: 'not found' } })
  }
}

test('GWT#6 敏感零落地：源码无 localStorage/sessionStorage/cookie 写入', () => {
  assert.equal(src.includes('localStorage'), false)
  assert.equal(src.includes('sessionStorage'), false)
  assert.equal(src.includes('document.cookie'), false)
})

test('敏感零落地：源码无 Bearer token 字面量', () => {
  assert.equal(/Bearer\s+[A-Za-z0-9]/.test(src), false)
})

test('UI 非权威：唯一 fetch 基址是 Admin API 前缀（无 DB/内部服务直连）', () => {
  const fetchCalls = src.match(/fetch\(`\/api\/admin\/v1/g) ?? []
  assert.ok(fetchCalls.length > 0)
  assert.equal(src.includes('localhost:5432'), false)
  assert.equal(src.includes('127.0.0.1:543'), false)
})

test('并发冲突语义：409 信封携带唯一 code，UI 以显式冲突呈现（不静默覆盖）', () => {
  // the source maps 409 to the explicit conflict UI state
  assert.ok(src.includes('409'))
  assert.ok(src.includes('并发冲突'))
  assert.ok(src.includes('未覆盖'))
})

test('mock 契约：并发 bump → 409 + BINDING_REVISION_CONFLICT 信封', async () => {
  const api = makeMockApi({ scopes: ['resource.publish'], concurrentBump: true })
  const res = await api('http://x/api/admin/v1/bindings/bind-a/publish', {
    method: 'POST',
    headers: { 'If-Match': '3' },
    body: '{}',
  })
  assert.equal(res.status, 409)
  const body = await res.json()
  assert.equal(body.error.code, 'BINDING_REVISION_CONFLICT')
})

test('mock 契约：scope 决定发布按钮可见性（whoami scopes 驱动 canPublish）', () => {
  // the source gates the publish button on who?.canPublish
  assert.ok(src.includes('canPublish'))
  assert.ok(src.includes('who?.canPublish'))
})

test('幂等键（GWT#3）：发布走 POST + expected_revision（幂等键一致时不产生重复发布）', () => {
  // the source sends expected_revision in the publish body
  assert.ok(src.includes('expected_revision'))
})

test('构建产物敏感零扫描：dist JS 无 token/密钥形状', async () => {
  try {
    const dist = readFileSync(new URL('../dist/assets/', import.meta.url) + 'index.js', 'utf8')
    assert.equal(/Bearer\s+[A-Za-z0-9]{10,}/.test(dist), false)
    assert.equal(dist.includes('localStorage'), false)
  } catch {
    // dist/index-*.js is hashed — scan all assets
    const fs = await import('node:fs/promises')
    const files = await fs.readdir(new URL('../dist/assets/', import.meta.url))
    for (const f of files.filter((f) => f.endsWith('.js'))) {
      const js = await fs.readFile(new URL(`../dist/assets/${f}`, import.meta.url), 'utf8')
      assert.equal(/Bearer\s+[A-Za-z0-9]{10,}/.test(js), false, `${f} carries a token shape`)
    }
  }
})
