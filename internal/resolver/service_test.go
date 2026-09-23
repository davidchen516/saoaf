package resolver

// Service-level real-DB tests (I09): the full deterministic pipeline with
// the FIXED error routing order, scope/policy filtering, snapshot validity,
// idempotency end-to-end, and determinism regression.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidchen516/saoaf/internal/policy"
)

const (
	testCapability = "model.test.reasoning"
	testProfile    = "test-profile-v1"
)

type seedIDs struct {
	CapID      int64
	ProviderID int64
	SnapshotID int64
}

func seedFullStack(t *testing.T, dsn string) seedIDs {
	t.Helper()
	conn := mustConnR(t, dsn)
	ctx := context.Background()

	var ids seedIDs
	// capability (PUBLISHED)
	if err := conn.QueryRow(ctx, `
		INSERT INTO registry.capability_definition
			(capability_key, major_version, revision, resource_type, requirement_schema, state, owner_ref)
		VALUES ($1, 1, 1, 'MODEL', '{}', 'PUBLISHED', 'user:test')
		RETURNING id`, testCapability).Scan(&ids.CapID); err != nil {
		t.Fatalf("seed capability: %v", err)
	}
	// provider (PUBLISHED)
	if err := conn.QueryRow(ctx, `
		INSERT INTO registry.resource_provider
			(provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
		VALUES ('prov-test', 'MODEL', 'svc://test/mmr', 'user:test', 'spiffe://saoaf.test/ns/default/sa/mmr',
		        'PUBLISHED', 1, 1)
		RETURNING id`).Scan(&ids.ProviderID); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	// snapshot (PUBLISHED, valid 1h, one AVAILABLE profile serving the capability)
	profiles := fmt.Sprintf(`[{"profile_id":%q,"capability_keys":[%q],"regions":["cn-east"],
		"data_classification_max":"CONFIDENTIAL","status":"AVAILABLE"}]`, testProfile, testCapability)
	if err := conn.QueryRow(ctx, `
		INSERT INTO registry.provider_snapshot
			(provider_id, snapshot_version, contract_version, digest, signature, workload_identity,
			 profiles, generated_at, valid_until, state)
		VALUES ($1, 1, '2026.09', 'sha256:1111111111111111111111111111111111111111111111111111111111111111', 'sig',
		        'spiffe://saoaf.test/ns/default/sa/mmr', $2::jsonb, now(), now() + interval '1 hour', 'PUBLISHED')
		RETURNING id`, ids.ProviderID, profiles).Scan(&ids.SnapshotID); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	// active pointer
	if _, err := conn.Exec(ctx, `
		INSERT INTO registry.provider_active_pointer (provider_id, snapshot_id)
		VALUES ($1, $2)`, ids.ProviderID, ids.SnapshotID); err != nil {
		t.Fatalf("seed pointer: %v", err)
	}
	return ids
}

func seedPolicySet(t *testing.T, dsn, setID string, expr string) {
	t.Helper()
	conn := mustConnR(t, dsn)
	ctx := context.Background()
	c := policy.Content{EligibilityCEL: expr}
	if _, err := conn.Exec(ctx, `
		INSERT INTO policy.policy_revision
			(set_id, version, state, content, content_digest, published_at, activated_at)
		VALUES ($1, 1, 'ACTIVATED', $2::jsonb, $3, now(), now())`,
		setID, fmt.Sprintf(`{"regions":[],"data_classification_max":"","vendor_restrictions":[],"export_requirements":[],"eligibility_cel":%q}`, expr), c.CanonicalDigest()); err != nil {
		t.Fatalf("seed policy: %v", err)
	}
}

func newService(db string, policySetID string) *Service {
	pool, err := pgxpool.New(context.Background(), db)
	if err != nil {
		panic(err)
	}
	cache := NewSnapshotCache(NewRegistrySnapshotLoader(pool), 5*time.Second)
	return &Service{
		Plans:       &Store{Pool: pool},
		Cache:       cache,
		Pool:        pool,
		Policy:      &policy.Store{DSN: db, Pool: pool},
		Eval:        policy.NewEvaluator(),
		PolicySetID: policySetID,
		Now:         time.Now,
		NewID:       func() string { return fmt.Sprintf("plan-%d", time.Now().UnixNano()) },
		DefaultTTL:  300 * time.Second,
		MaxTTL:      3600 * time.Second,
	}
}

func resolveReq() *Request {
	return &Request{
		ContractVersion: "1.0",
		TaskRef:         "task-test-1",
		Requirements: []Requirement{{
			RequirementID:          "req-1",
			CapabilityID:           testCapability,
			CapabilityMajorVersion: 1,
			ResourceType:           "MODEL_PROVIDER",
			Constraints: map[string]any{
				"region":                  "cn-east",
				"data_classification_max": "CONFIDENTIAL",
			},
		}},
	}
}

func callerMeta() CallerMeta {
	return CallerMeta{CallerRef: "user:tester", TenantRef: "tenant-x", Environment: "production", TraceID: "trace-1"}
}

// Happy path：确定性 Plan + fingerprint 可复验。
func TestDBResolveHappyPath(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		seedActiveBinding(t, db, ids, "bind-main", allScope(), 100)
		svc := newService(db, "")

		plan, created, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-h1")
		if err != nil || !created {
			t.Fatalf("resolve: created=%v err=%v", created, err)
		}
		if plan.Status != StatusResolved || len(plan.Items) != 1 {
			t.Fatalf("plan = %s items=%d", plan.Status, len(plan.Items))
		}
		it := plan.Items[0]
		if it.BindingKey != "bind-main" || it.SnapshotVersion != 1 || it.ProfileOrAction != testProfile {
			t.Fatalf("item = %+v", it)
		}
		if it.ProviderKey != "prov-test" || it.EndpointRef != "svc://test/mmr" {
			t.Fatalf("provider info = %+v", it)
		}
		if len(it.ReasonCodes) == 0 {
			t.Fatal("no reason codes")
		}
		// determinism: same input, different idempotency key → same fingerprint
		plan2, _, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-h2")
		if err != nil {
			t.Fatalf("second resolve: %v", err)
		}
		if plan2.Fingerprint != plan.Fingerprint {
			t.Fatalf("fingerprint drifted: %s vs %s", plan.Fingerprint, plan2.Fingerprint)
		}
		// GWT#1: fingerprint 可复验
		if fp := Fingerprint(resolveReq(), []RevisionInput{{
			CapabilityKey: testCapability, CapabilityRevision: 1,
			BindingKey: "bind-main", BindingRevision: 1, ProviderKey: "prov-test",
			SnapshotVersion: 1, Profile: testProfile,
		}}, "", 0); fp != plan.Fingerprint {
			t.Fatalf("fingerprint not reproducible: %s vs %s", fp, plan.Fingerprint)
		}
	})
}

func seedActiveBinding(t *testing.T, dsn string, ids seedIDs, key string, scope stringJSONScope, priority int) {
	t.Helper()
	conn := mustConnR(t, dsn)
	_, err := conn.Exec(context.Background(), `
		INSERT INTO registry.capability_binding
			(binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
			 environment, scope, scope_hash, priority, state, revision, is_active)
		VALUES ($1, $2, $3, $4, $5, 'production', $6::jsonb, $7, $8, 'PUBLISHED', 1, TRUE)`,
		key, ids.CapID, ids.ProviderID, ids.SnapshotID, testProfile,
		scope.scope, scope.hash, priority)
	if err != nil {
		t.Fatalf("seed binding %s: %v", key, err)
	}
}

type stringJSONScope struct {
	scope string
	hash  string
}

func allScope() stringJSONScope {
	return stringJSONScope{scope: `{"tenant_refs":[],"factory_refs":[],"regions":[],"agent_refs":[]}`,
		hash: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
}

// CAPABILITY_NOT_FOUND（404）。
func TestDBResolveCapabilityNotFound(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		seedActiveBinding(t, db, ids, "bind-x", allScope(), 100)
		svc := newService(db, "")
		req := resolveReq()
		req.Requirements[0].CapabilityID = "model.missing"
		_, _, err := svc.Resolve(context.Background(), req, callerMeta(), "idem-nf")
		if err == nil || err.(*ResolveError).Code != CodeCapabilityNotFound {
			t.Fatalf("want CAPABILITY_NOT_FOUND, got %v", err)
		}
	})
}

// NO_COMPATIBLE_PROVIDER（422）：请求 region 与 profile/binding 不匹配。
func TestDBResolveNoCompatibleRegion(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		// binding restricted to cn-north; request asks cn-east
		seedActiveBinding(t, db, ids, "bind-north",
			stringJSONScope{scope: `{"tenant_refs":[],"factory_refs":[],"regions":["cn-north"],"agent_refs":[]}`,
				hash: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}, 100)
		svc := newService(db, "")
		_, _, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-rg")
		if err == nil || err.(*ResolveError).Code != CodeNoCompatibleProvider {
			t.Fatalf("want NO_COMPATIBLE_PROVIDER, got %v", err)
		}
	})
}

// scope 过滤：caller 租户不在 tenant_refs → 无候选（422）。
func TestDBResolveScopeFiltered(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		seedActiveBinding(t, db, ids, "bind-other",
			stringJSONScope{scope: `{"tenant_refs":["tenant-other"],"factory_refs":[],"regions":[],"agent_refs":[]}`,
				hash: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}, 100)
		svc := newService(db, "")
		_, _, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-sc")
		if err == nil || err.(*ResolveError).Code != CodeNoCompatibleProvider {
			t.Fatalf("want NO_COMPATIBLE_PROVIDER (scope), got %v", err)
		}
	})
}

// AMBIGUOUS_BINDING（409）：同优先级两个匹配 binding（防御性检测；I08 的
// overlap guard 使经 API 无法造出，故此处直接 SQL 打破约束模拟历史数据）。
func TestDBResolveAmbiguousBinding(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		seedActiveBinding(t, db, ids, "bind-a1",
			stringJSONScope{scope: `{"tenant_refs":["tenant-x"],"factory_refs":[],"regions":[],"agent_refs":[]}`,
				hash: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}, 100)
		seedActiveBinding(t, db, ids, "bind-a2",
			stringJSONScope{scope: `{"tenant_refs":["tenant-x","tenant-y"],"factory_refs":[],"regions":[],"agent_refs":[]}`,
				hash: "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}, 100)
		svc := newService(db, "")
		_, _, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-amb")
		if err == nil || err.(*ResolveError).Code != CodeAmbiguousBinding {
			t.Fatalf("want AMBIGUOUS_BINDING, got %v", err)
		}
	})
}

// 优先级消歧：同请求下不同优先级 → 低优先级数字（更高优先）胜出。
func TestDBResolvePriorityWins(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		// 两个 binding 都匹配，但优先级不同（无 overlap 约束冲突：scope 不相交？
		// 不——同 tenant 会 overlap 且同优先级才被 I08 挡；不同优先级合法）
		seedActiveBinding(t, db, ids, "bind-low",
			stringJSONScope{scope: `{"tenant_refs":["tenant-x"],"factory_refs":[],"regions":[],"agent_refs":[]}`,
				hash: "sha256:1111111111111111111111111111111111111111111111111111111111111112"}, 200)
		seedActiveBinding(t, db, ids, "bind-high",
			stringJSONScope{scope: `{"tenant_refs":["tenant-x"],"factory_refs":[],"regions":[],"agent_refs":[]}`,
				hash: "sha256:1111111111111111111111111111111111111111111111111111111111111113"}, 50)
		svc := newService(db, "")
		plan, _, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-prio")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.Items[0].BindingKey != "bind-high" {
			t.Fatalf("winner = %s, want bind-high (priority 50)", plan.Items[0].BindingKey)
		}
	})
}

// PROVIDER_SNAPSHOT_EXPIRED（424）：valid_until 已过。
func TestDBResolveSnapshotExpired(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		conn := mustConnR(t, db)
		if _, err := conn.Exec(context.Background(),
			`UPDATE registry.provider_snapshot
			 SET generated_at = now() - interval '2 minute', valid_until = now() - interval '1 minute'
			 WHERE id = $1`,
			ids.SnapshotID); err != nil {
			t.Fatal(err)
		}
		seedActiveBinding(t, db, ids, "bind-stale", allScope(), 100)
		svc := newService(db, "")
		_, _, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-exp")
		if err == nil || err.(*ResolveError).Code != CodeSnapshotExpired {
			t.Fatalf("want PROVIDER_SNAPSHOT_EXPIRED, got %v", err)
		}
	})
}

// PROVIDER_SNAPSHOT_EXPIRED（424）：active pointer 已移到新 snapshot，
// binding 仍钉住旧的（必须重发布 binding，而不是静默用旧 provider 内容）。
func TestDBResolveSnapshotNotActive(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		seedActiveBinding(t, db, ids, "bind-pin", allScope(), 100)
		// publish snapshot v2 and move the pointer
		if _, err := mustConnR(t, db).Exec(context.Background(), `
			INSERT INTO registry.provider_snapshot
				(provider_id, snapshot_version, contract_version, digest, signature, workload_identity,
				 profiles, generated_at, valid_until, state)
			VALUES ($1, 2, '2026.09', 'sha256:2222222222222222222222222222222222222222222222222222222222222222', 'sig',
			        'spiffe://saoaf.test/ns/default/sa/mmr',
			        '[{"profile_id":"p2","capability_keys":[],"regions":[],"status":"AVAILABLE"}]'::jsonb,
			        now(), now() + interval '2 hour', 'PUBLISHED')`, ids.ProviderID); err != nil {
			t.Fatal(err)
		}
		if _, err := mustConnR(t, db).Exec(context.Background(), `
			UPDATE registry.provider_active_pointer SET snapshot_id = (
				SELECT id FROM registry.provider_snapshot WHERE provider_id = $1 AND snapshot_version = 2)
			WHERE provider_id = $1`, ids.ProviderID); err != nil {
			t.Fatal(err)
		}
		svc := newService(db, "")
		_, _, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-ptr")
		if err == nil || err.(*ResolveError).Code != CodeSnapshotExpired {
			t.Fatalf("want PROVIDER_SNAPSHOT_EXPIRED (pointer moved), got %v", err)
		}
	})
}

// GWT#8（I08 挂账落地）：Provider SUSPENDED 后不再被新 Plan 选中。
func TestDBResolveSuspendedProviderFiltered(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		if _, err := mustConnR(t, db).Exec(context.Background(),
			`UPDATE registry.resource_provider SET state = 'SUSPENDED' WHERE id = $1`,
			ids.ProviderID); err != nil {
			t.Fatal(err)
		}
		seedActiveBinding(t, db, ids, "bind-susp", allScope(), 100)
		svc := newService(db, "")
		_, _, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-susp")
		if err == nil || err.(*ResolveError).Code != CodeNoCompatibleProvider {
			t.Fatalf("want NO_COMPATIBLE_PROVIDER (suspended provider), got %v", err)
		}
	})
}

// Policy 过滤（携带 policy revision）：deny 表达式 → POLICY_DENIED。
func TestDBResolvePolicyDenied(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		seedActiveBinding(t, db, ids, "bind-pol", allScope(), 100)
		seedPolicySet(t, db, "resolver-eligibility", "false")
		svc := newService(db, "resolver-eligibility")
		_, _, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-pol")
		if err == nil {
			t.Fatal("policy denial accepted")
		}
		re := err.(*ResolveError)
		if re.Code != CodeNoCompatibleProvider || re.Details[0].ReasonCodes[0] != ReasonPolicyDenied {
			t.Fatalf("want NO_COMPATIBLE_PROVIDER/POLICY_DENIED, got %+v", re)
		}
	})
}

// Policy 允许时，Plan 携带 policy set + version，fingerprint 随之变化。
func TestDBResolvePolicyRevisionCarried(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		seedActiveBinding(t, db, ids, "bind-pol-ok", allScope(), 100)
		seedPolicySet(t, db, "resolver-eligibility", "true")
		svc := newService(db, "resolver-eligibility")
		plan, _, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-pol-ok")
		if err != nil {
			t.Fatalf("resolve with policy: %v", err)
		}
		if plan.PolicySetID != "resolver-eligibility" || plan.PolicyVersion != 1 {
			t.Fatalf("policy revision not carried: %s@%d", plan.PolicySetID, plan.PolicyVersion)
		}
	})
}

// 幂等端到端：同 key 重放返回同一 Plan；不同请求体同 key → 409。
func TestDBResolveIdempotencyEndToEnd(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		seedActiveBinding(t, db, ids, "bind-idem", allScope(), 100)
		svc := newService(db, "")
		p1, c1, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-e2e")
		if err != nil || !c1 {
			t.Fatalf("first: %v", err)
		}
		p2, c2, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-e2e")
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if c2 || p2.ID != p1.ID {
			t.Fatalf("replay created=%v id=%s, want same plan %s", c2, p2.ID, p1.ID)
		}
		req2 := resolveReq()
		req2.TaskRef = "task-different"
		_, _, err = svc.Resolve(context.Background(), req2, callerMeta(), "idem-e2e")
		if err == nil || err.(*ResolveError).Code != CodeIdempotencyConflict {
			t.Fatalf("want IDEMPOTENCY_CONFLICT, got %v", err)
		}
	})
}

// 100 次相同 resolve 并发：恰一个 Plan，全部同 ID（GWT#3 端到端）。
func TestDBResolve100ConcurrentOnePlan(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		seedActiveBinding(t, db, ids, "bind-c100", allScope(), 100)
		svc := newService(db, "")
		var created atomic.Int64
		var wg sync.WaitGroup
		var firstID atomic.Value
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				plan, ok, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-c100")
				if err != nil {
					t.Errorf("concurrent resolve: %v", err)
					return
				}
				if ok {
					created.Add(1)
				}
				firstID.CompareAndSwap(nil, plan.ID)
				if plan.ID != firstID.Load().(string) {
					t.Errorf("plan id drifted: %s vs %s", plan.ID, firstID.Load())
				}
			}()
		}
		wg.Wait()
		if created.Load() != 1 {
			t.Fatalf("created = %d, want exactly 1", created.Load())
		}
		conn := mustConnR(t, db)
		var rows int
		if err := conn.QueryRow(context.Background(),
			`SELECT count(*) FROM resolver.resource_plan`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("plan rows = %d, want 1", rows)
		}
	})
}

// TTL：默认 300s；options 可缩短；plan 过期后 Get 返回 EXPIRED 前需 sweep。
func TestDBResolveTTL(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		seedActiveBinding(t, db, ids, "bind-ttl", allScope(), 100)
		svc := newService(db, "")
		req := resolveReq()
		req.Options.MaxPlanTTLSeconds = 60
		plan, _, err := svc.Resolve(context.Background(), req, callerMeta(), "idem-ttl")
		if err != nil {
			t.Fatal(err)
		}
		got, err := time.Parse(time.RFC3339Nano, plan.ExpiresAt)
		if err != nil {
			t.Fatal(err)
		}
		created, _ := time.Parse(time.RFC3339Nano, plan.CreatedAt)
		if got.Sub(created) != 60*time.Second {
			t.Fatalf("TTL = %v, want 60s", got.Sub(created))
		}
	})
}
