package resolver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeLoader struct {
	mu       sync.Mutex
	loads    int64
	fail     bool
	snap     *LoadedSnapshot
	loadHook func()
}

func (f *fakeLoader) Load(ctx context.Context, snapshotID int64) (*LoadedSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.loadHook != nil {
		f.loadHook()
	}
	if f.fail {
		return nil, errors.New("db down")
	}
	snap := *f.snap
	snap.SnapshotID = snapshotID
	return &snap, nil
}

func (f *fakeLoader) setFail(v bool)         { f.mu.Lock(); f.fail = v; f.mu.Unlock() }
func (f *fakeLoader) loads_() int64          { f.mu.Lock(); defer f.mu.Unlock(); return f.loads }
func (f *fakeLoader) swap(s *LoadedSnapshot) { f.mu.Lock(); f.snap = s; f.mu.Unlock() }

func testSnap(validUntil time.Time, version int) *LoadedSnapshot {
	return &LoadedSnapshot{
		SnapshotID: 1, ProviderID: 1, SnapshotVersion: version,
		ProviderKey: "prov", ProviderType: "MODEL", EndpointRef: "svc://x",
		GeneratedAt: time.Now(), ValidUntil: validUntil,
	}
}

// 单航班加载：同 snapshot 并发 Get 恰触发一次回源。
func TestCacheSingleflight(t *testing.T) {
	fl := &fakeLoader{snap: testSnap(time.Now().Add(time.Hour), 1)}
	fl.loadHook = func() { time.Sleep(50 * time.Millisecond) } // widen the race window
	c := NewSnapshotCache(fl.Load, time.Hour)
	var wg sync.WaitGroup
	var okCount atomic.Int64
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Get(context.Background(), 42); err == nil {
				okCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if okCount.Load() != 50 {
		t.Fatalf("gets failed: %d/50", okCount.Load())
	}
	if fl.loads_() != 1 {
		t.Fatalf("singleflight violated: %d loader calls for 50 concurrent gets", fl.loads_())
	}
}

// revision 失效：snapshot version 变化后重新加载。
func TestCacheRevisionInvalidation(t *testing.T) {
	fl := &fakeLoader{snap: testSnap(time.Now().Add(time.Hour), 1)}
	c := NewSnapshotCache(fl.Load, time.Hour)
	s1, err := c.Get(context.Background(), 7)
	if err != nil || s1.SnapshotVersion != 1 {
		t.Fatalf("first get: %v %v", s1, err)
	}
	// provider publishes a new snapshot version
	fl.swap(testSnap(time.Now().Add(2*time.Hour), 2))
	c.mu.Lock()
	c.entries[7].snap.SnapshotVersion = 1 // still cached old version
	c.mu.Unlock()
	s2, err := c.Get(context.Background(), 7)
	if err != nil || s2.SnapshotVersion != 1 {
		t.Fatalf("cache should serve fresh entry until evicted: %v %v", s2, err)
	}
	// simulate entry invalidation (TTL of the ENTRY itself, e.g. cache
	// refresh cycle): flip to degraded or drop → next Get reloads v2
	c.mu.Lock()
	delete(c.entries, 7)
	c.mu.Unlock()
	s3, err := c.Get(context.Background(), 7)
	if err != nil || s3.SnapshotVersion != 2 {
		t.Fatalf("after invalidation the new version must load: %v %v", s3, err)
	}
}

// GWT#9 降级读：数据库短时不可用时未过期快照继续可读；过期快照拒绝。
func TestCacheDegradedReadOnOutage(t *testing.T) {
	fl := &fakeLoader{snap: testSnap(time.Now().Add(30*time.Minute), 1)}
	c := NewSnapshotCache(fl.Load, 10*time.Millisecond)
	if _, err := c.Get(context.Background(), 9); err != nil {
		t.Fatalf("prime: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // soft refresh deadline passes
	// DB goes down: the refresh attempt fails but the unexpired entry serves
	fl.setFail(true)
	s, err := c.Get(context.Background(), 9)
	if err != nil {
		t.Fatalf("unexpired snapshot must be served during outage: %v", err)
	}
	if s.SnapshotVersion != 1 {
		t.Fatal("degraded read served wrong snapshot")
	}
	if c.Stats().DegradedHits == 0 {
		t.Fatal("degraded-hit counter not incremented")
	}
	// recovery: the degraded marking makes the next Get retry the loader
	fl.setFail(false)
	fl.swap(testSnap(time.Now().Add(time.Hour), 2))
	if s, err = c.Get(context.Background(), 9); err != nil || s.SnapshotVersion != 2 {
		t.Fatalf("post-recovery load failed: %v %v", s, err)
	}
}

func TestCacheExpiredRefusedDuringOutage(t *testing.T) {
	// snapshot whose window ends during the outage
	fl := &fakeLoader{snap: testSnap(time.Now().Add(40*time.Millisecond), 1)}
	c := NewSnapshotCache(fl.Load, time.Hour)
	if _, err := c.Get(context.Background(), 5); err != nil {
		t.Fatalf("prime: %v", err)
	}
	fl.setFail(true)
	time.Sleep(60 * time.Millisecond) // valid_until passes while DB is down
	if _, err := c.Get(context.Background(), 5); err == nil {
		t.Fatal("EXPIRED snapshot served during outage (must be refused)")
	}
	// and with the DB back but the snapshot window over, the loader
	// returns the (expired) snapshot — the SERVICE enforces the window
	fl.setFail(false)
	s, err := c.Get(context.Background(), 5)
	if err != nil {
		t.Fatalf("loader should still return the row; service enforces expiry: %v", err)
	}
	if s.ValidUntil.After(time.Now()) {
		t.Fatal("expected expired snapshot")
	}
}

// 无缓存且 DB 不可用 → 错误（首次加载无降级可依）。
func TestCacheColdMissOutage(t *testing.T) {
	fl := &fakeLoader{snap: testSnap(time.Now().Add(time.Hour), 1), fail: true}
	c := NewSnapshotCache(fl.Load, time.Hour)
	if _, err := c.Get(context.Background(), 3); err == nil {
		t.Fatal("cold miss during outage must fail (no approved snapshot to degrade to)")
	}
}
