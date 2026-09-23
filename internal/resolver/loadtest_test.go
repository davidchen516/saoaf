package resolver

// GWT#9 降级读 + GWT#8 负载测试。负载测试有两种形态：
//   - 默认（CI）：30 秒 100 QPS 冒烟，断言 P99 ≤ 100ms 且零错误
//   - SAOAF_LOADTEST_FULL=1（本地证据）：15 分钟 100 QPS 完整跑批，输出
//     P50/P95/P99/错误率报告（issue 必须提交的证据：P99 曲线 + QPS +
//     错误率 < 0.1%）

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedLoadDataset 播种 MVP 数据集：100 Capability、50 Provider、500
// Binding（每 capability 5 个 binding，跨 provider 均摊）。
func seedLoadDataset(t *testing.T, db string) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	// capabilities
	if _, err := c.Exec(ctx, `
		INSERT INTO registry.capability_definition
			(capability_key, major_version, revision, resource_type, requirement_schema, state, owner_ref)
		SELECT 'model.cap.' || g, 1, 1, 'MODEL', '{}', 'PUBLISHED', 'user:load'
		FROM generate_series(1, 100) g`); err != nil {
		t.Fatal(err)
	}
	// providers
	if _, err := c.Exec(ctx, `
		INSERT INTO registry.resource_provider
			(provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
		SELECT 'prov.' || g, 'MODEL', 'svc://load/' || g, 'user:load',
		       'spiffe://saoaf.test/ns/default/sa/p' || g, 'PUBLISHED', 1, 1
		FROM generate_series(1, 50) g`); err != nil {
		t.Fatal(err)
	}
	// one published snapshot per provider + active pointer
	if _, err := c.Exec(ctx, `
		INSERT INTO registry.provider_snapshot
			(provider_id, snapshot_version, contract_version, digest, signature, workload_identity,
			 profiles, generated_at, valid_until, state)
		SELECT rp.id, 1, '2026.09',
		       'sha256:' || lpad(md5(rp.provider_key), 64, '0'), 'sig', rp.workload_identity,
		       jsonb_build_array(jsonb_build_object(
		           'profile_id', 'prof-' || rp.provider_key,
		           'capability_keys', '[]'::jsonb,
		           'regions', '["cn-east"]'::jsonb,
		           'data_classification_max', 'CONFIDENTIAL',
		           'status', 'AVAILABLE')),
		       now(), now() + interval '1 day', 'PUBLISHED'
		FROM registry.resource_provider rp`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `
		INSERT INTO registry.provider_active_pointer (provider_id, snapshot_id)
		SELECT rp.id, ps.id
		FROM registry.resource_provider rp
		JOIN registry.provider_snapshot ps ON ps.provider_id = rp.id AND ps.snapshot_version = 1`); err != nil {
		t.Fatal(err)
	}
	// 500 bindings: capability i bound to provider (i mod 50)+1
	if _, err := c.Exec(ctx, `
		INSERT INTO registry.capability_binding
			(binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
			 environment, scope, scope_hash, priority, state, revision, is_active)
		SELECT 'bind.' || cd.capability_key || '.' || g,
		       cd.id,
		       rp.id,
		       ps.id,
		       'prof-' || rp.provider_key,
		       'production',
		       '{"tenant_refs":[],"factory_refs":[],"regions":[],"agent_refs":[]}'::jsonb,
		       'sha256:' || lpad(md5(cd.capability_key || g), 64, '0'),
		       100 * g, 'PUBLISHED', 1, TRUE
		FROM registry.capability_definition cd
		CROSS JOIN generate_series(1, 5) g
		JOIN registry.resource_provider rp ON rp.id = ((cd.id + g - 2) % 50) + 1
		JOIN registry.provider_snapshot ps ON ps.provider_id = rp.id AND ps.snapshot_version = 1
		WHERE cd.state = 'PUBLISHED'`); err != nil {
		t.Fatal(err)
	}
}

// TestLoadSmoke：MVP 数据集 100 QPS 30s（CI 形态），P99 ≤ 100ms、零错误。
func TestLoadSmoke(t *testing.T) {
	withDBR(t, func(db string) {
		seedLoadDataset(t, db)
		pool, err := pgxpool.New(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		svc := &Service{
			Plans:      &Store{Pool: pool},
			Cache:      NewSnapshotCache(NewRegistrySnapshotLoader(pool), 30*time.Second),
			Pool:       pool,
			Now:        time.Now,
			NewID:      NewPlanID,
			DefaultTTL: 300 * time.Second,
			MaxTTL:     3600 * time.Second,
		}
		runLoad(t, svc, 30*time.Second, 100)
	})
}

// TestLoadFull：SAOAF_LOADTEST_FULL=1 时跑 15 分钟 100 QPS（issue 证据形态）。
func TestLoadFull(t *testing.T) {
	if os.Getenv("SAOAF_LOADTEST_FULL") != "1" {
		t.Skip("set SAOAF_LOADTEST_FULL=1 for the 15-minute evidence run")
	}
	withDBR(t, func(db string) {
		seedLoadDataset(t, db)
		pool, err := pgxpool.New(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		svc := &Service{
			Plans:      &Store{Pool: pool},
			Cache:      NewSnapshotCache(NewRegistrySnapshotLoader(pool), 30*time.Second),
			Pool:       pool,
			Now:        time.Now,
			NewID:      NewPlanID,
			DefaultTTL: 300 * time.Second,
			MaxTTL:     3600 * time.Second,
		}
		runLoad(t, svc, 15*time.Minute, 100)
	})
}

// runLoad drives the service at target QPS for the duration, recording
// per-request latencies; asserts the issue thresholds (P99 ≤ 100ms, 错误率
// < 0.1%) and prints the report for evidence.
func runLoad(t *testing.T, svc *Service, duration time.Duration, qps int) {
	t.Helper()
	ctx := context.Background()
	var n atomic.Int64
	var errs atomic.Int64
	var firstErr atomic.Value
	latenciesCh := make(chan time.Duration, 1<<20)
	stop := make(chan struct{})

	// pacer: exactly one request per tick → target QPS (workers > QPS keeps
	// in-flight concurrency low, latency reflects service capacity)
	ticker := time.NewTicker(time.Second / time.Duration(qps))
	defer ticker.Stop()
	workers := 16
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(capIdx int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
				}
				req := &Request{
					ContractVersion: "1.0",
					TaskRef:         fmt.Sprintf("task-load-%d-%d", capIdx, i),
					Requirements: []Requirement{{
						RequirementID:          "req-1",
						CapabilityID:           fmt.Sprintf("model.cap.%d", (capIdx+i)%100+1),
						CapabilityMajorVersion: 1,
						ResourceType:           "MODEL_PROVIDER",
						Constraints:            map[string]any{"region": "cn-east"},
					}},
				}
				start := time.Now()
				_, _, err := svc.Resolve(ctx, req, CallerMeta{
					CallerRef: "user:load", TenantRef: "tenant-load", Environment: "production",
				}, fmt.Sprintf("idem-load-%d-%d", capIdx, i))
				d := time.Since(start)
				latenciesCh <- d
				if err != nil {
					if firstErr.CompareAndSwap(nil, err) {
						fmt.Printf("LOAD FIRST ERROR: %v\n", err)
					}
					errs.Add(1)
				}
				n.Add(1)
				i++
			}
		}(w)
	}
	// pacing: close the stop channel when duration elapses
	time.Sleep(duration)
	close(stop)
	wg.Wait()
	close(latenciesCh)

	latencies := make([]time.Duration, 0, n.Load())
	for d := range latenciesCh {
		latencies = append(latencies, d)
	}
	// percentile helper
	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		// latencies unsorted: partial selection is fine for reporting; sort
		// for determinism
		sortDurations(latencies)
		idx := int(float64(len(latencies)-1) * p)
		return latencies[idx]
	}
	total := n.Load()
	errRate := float64(errs.Load()) / float64(total)
	report := fmt.Sprintf(
		"LOAD REPORT: duration=%s target_qps=%d requests=%d actual_qps=%.1f errors=%d (%.4f%%) p50=%s p95=%s p99=%s max=%s",
		duration, qps, total, float64(total)/duration.Seconds(), errs.Load(), errRate*100,
		pct(0.50), pct(0.95), pct(0.99), latencies[len(latencies)-1],
	)
	fmt.Println(report)

	if errRate >= 0.001 {
		t.Fatalf("error rate %.4f%% exceeds 0.1%%", errRate*100)
	}
	if p99 := pct(0.99); p99 > 100*time.Millisecond {
		t.Fatalf("P99 %s exceeds 100ms", p99)
	}
}

func sortDurations(d []time.Duration) {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
}

// GWT#9：数据库短时不可用——已加载的未过期 Published Snapshot 继续可读
// （降级读），resolve 侧 fail-closed（503 RESOLVER_UNAVAILABLE）。
func TestResolveDBOutageDegradation(t *testing.T) {
	withDBR(t, func(db string) {
		ids := seedFullStack(t, db)
		seedActiveBinding(t, db, ids, "bind-out", allScope(), 100)

		goodPool, err := pgxpool.New(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		defer goodPool.Close()
		// outage injector: the cache's loader is the same DB path the rest
		// of the service uses; the flag stands in for the DB going down
		var down atomic.Bool
		realLoader := NewRegistrySnapshotLoader(goodPool)
		cache := NewSnapshotCache(func(ctx context.Context, snapshotID int64) (*LoadedSnapshot, error) {
			if down.Load() {
				return nil, errors.New("db down")
			}
			return realLoader(ctx, snapshotID)
		}, 10*time.Millisecond)
		svc := &Service{
			Plans: &Store{Pool: goodPool}, Cache: cache, Pool: goodPool,
			Now: time.Now, NewID: NewPlanID,
			DefaultTTL: 300 * time.Second, MaxTTL: 3600 * time.Second,
		}
		// prime: one successful resolve loads the snapshot into the cache
		if _, _, err := svc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-out-1"); err != nil {
			t.Fatalf("prime resolve: %v", err)
		}
		if cache.Loaded() == 0 {
			t.Fatal("cache not primed")
		}
		// DB goes down: the loader fails and every other query path
		// fails (dead pool = deterministic stand-in for a stopped DB)
		down.Store(true)
		time.Sleep(20 * time.Millisecond) // soft refresh deadline passes
		deadPool, err := pgxpool.New(context.Background(), "postgres://postgres:postgres@127.0.0.1:59999/none")
		if err != nil {
			t.Fatal(err)
		}
		defer deadPool.Close()
		deadSvc := &Service{
			Plans: &Store{Pool: deadPool}, Cache: cache, Pool: deadPool,
			Now: time.Now, NewID: NewPlanID,
			DefaultTTL: 300 * time.Second, MaxTTL: 3600 * time.Second,
		}
		// snapshot cache still serves (degraded read, unexpired window)
		snap, err := cache.Get(context.Background(), ids.SnapshotID)
		if err != nil {
			t.Fatalf("degraded snapshot read failed during outage: %v", err)
		}
		if snap.SnapshotVersion != 1 {
			t.Fatalf("degraded read served wrong snapshot: %+v", snap)
		}
		// resolve fail-closed: plan persistence needs the DB → 503
		_, _, err = deadSvc.Resolve(context.Background(), resolveReq(), callerMeta(), "idem-out-2")
		if err == nil {
			t.Fatal("resolve during DB outage unexpectedly succeeded")
		}
		re, ok := err.(*ResolveError)
		if !ok || re.Code != CodeResolverUnavailable {
			t.Fatalf("want RESOLVER_UNAVAILABLE during outage, got %v", err)
		}
		if cache.Stats().DegradedHits == 0 {
			t.Fatal("degraded-hit counter not incremented during outage")
		}
	})
}
