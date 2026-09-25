// I17 test suite (node:test — zero additional deps): source-level security
// and contract assertions for the Sovereignty Operations console.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const ops = readFileSync(new URL('../src/Ops.tsx', import.meta.url), 'utf8')
const app = readFileSync(new URL('../src/App.tsx', import.meta.url), 'utf8')

test('敏感零落地：Ops 源码无 localStorage/sessionStorage/cookie/Bearer', () => {
  assert.equal(ops.includes('localStorage'), false)
  assert.equal(ops.includes('sessionStorage'), false)
  assert.equal(ops.includes('document.cookie'), false)
  assert.equal(/Bearer\s+[A-Za-z0-9]/.test(ops), false)
})

test('UI 非权威：Ops 唯一 fetch 基址是 Admin API 前缀（无直连）', () => {
  // the single fetch site prefixes every path with the admin API base
  assert.ok(ops.includes('fetch(`/api/admin/v1${path}`)'))
  assert.equal(ops.includes('localhost:5432'), false)
  assert.equal(ops.includes('127.0.0.1:543'), false)
  // and every ops path the views build is relative (/ops/ endpoints)
  const paths = ops.match(/\/ops\/[a-z-]+/g) ?? []
  assert.ok(paths.length >= 5, 'ops views must target /ops/* endpoints')
})

test('AC: 服务端分页——每次请求携带 limit/offset，浏览器不加载全量', () => {
  // every list fetch passes explicit limit & offset query params
  assert.ok(ops.includes('limit=${limit}&offset=${offset}'))
  // no unbounded "load everything" pattern: no fetch without pagination
  // params on list views (overview is a fixed-size aggregate, exempt)
  assert.equal(ops.includes('ops/metrics?limit=200&offset=0'), false)
})

test('AC: 未知 ≠ 0——null 值渲染为显式「未知/数据不足」徽标，绝不当数值', () => {
  assert.ok(ops.includes('metric-unknown'))
  assert.ok(ops.includes("row.value !== null"))
  assert.ok(ops.includes('未知'))
  assert.ok(ops.includes('数据不足'))
  // the value cell only renders a number when the value is non-null
  assert.ok(ops.includes('row.value.toFixed'))
})

test('AC: 显式状态——StatusBadge 按状态着色（OK/UNKNOWN/INSUFFICIENT_DATA）', () => {
  assert.ok(ops.includes("status === 'OK'"))
  assert.ok(ops.includes("status === 'UNKNOWN'"))
  assert.ok(ops.includes("'INSUFFICIENT_DATA'"))
})

test('AC: 下钻三元组——每行渲染 formula_version / dataset_revision / evidence_ref', () => {
  assert.ok(ops.includes('drill-triple'))
  assert.ok(ops.includes('m.formula_version'))
  assert.ok(ops.includes('m.dataset_revision'))
  assert.ok(ops.includes('m.evidence_ref'))
  // missing evidence leg is EXPLICIT (em dash placeholder, not empty)
  assert.ok(ops.includes('无 Evidence 引用'))
})

test('AC: 证据断链视图——断链原因显式呈现（不计为健康）', () => {
  assert.ok(ops.includes('ops/metrics/broken'))
  assert.ok(ops.includes('broken_reason'))
  assert.ok(ops.includes('断链原因'))
})

test('AC: 过期 Exit Pack 读取时判定——EXPIRED 显式标注（不静默当健康）', () => {
  assert.ok(ops.includes("p.state === 'EXPIRED'"))
  assert.ok(ops.includes('已过期（读取时判定）'))
  // digest unknown stays explicit, not empty-string
  assert.ok(ops.includes('digest 未生成'))
})

test('AC: Drill 非法操作由 API 拒绝——Ops 是只读控制台（无写入口/无绕过路径）', () => {
  // read-only: no POST/PUT/DELETE anywhere in the ops console
  assert.equal(ops.includes('method: \'POST\''), false)
  assert.equal(ops.includes('method: \'PUT\''), false)
  assert.equal(ops.includes('method: \'DELETE\''), false)
  // the drill detail states the authority boundary
  assert.ok(ops.includes('API 的状态机裁决'))
  assert.ok(ops.includes('不提供绕过路径'))
})

test('AC: Drill 整改详情包含 findings + 状态机审计轨迹', () => {
  assert.ok(ops.includes('drill-detail'))
  assert.ok(ops.includes('findings'))
  assert.ok(ops.includes('drillLog'))
  assert.ok(ops.includes('from_state'))
  assert.ok(ops.includes('to_state'))
})

test('崩溃恢复：无本地缓存伪造——每次视图切换重新从 API 拉取', () => {
  // state resets to null on view change (setX(null) before load)
  assert.ok(ops.includes('setMetrics(null)'))
  assert.ok(ops.includes('setOverview(null)'))
  assert.ok(ops.includes('setDrills(null)'))
})

test('App 导航：Sovereignty Ops 入口存在', () => {
  assert.ok(app.includes('nav-ops'))
  assert.ok(app.includes("setView('ops')"))
})

test('冻结信封：Ops 解析 flat {error_code, message, request_id}', () => {
  assert.ok(ops.includes('error_code'))
  assert.ok(ops.includes('body.message'))
  assert.equal(ops.includes('body.error?.code'), false)
})
