package resolver

// Published Snapshot 内存缓存（I09 scope: Published Snapshot 内存缓存、
// revision 失效和单航班加载; GWT#9: 数据库短时不可用时未过期快照可按
// 批准策略继续读）。
//
// Semantics:
//   - revision 失效: entries are keyed by provider and carry the snapshot
//     version; a version bump (new active snapshot published) replaces the
//     entry on the next load.
//   - 单航班: concurrent misses for the same provider coalesce into one
//     loader invocation (golang.org/x/sync/singleflight).
//   - 降级读: entries carry a soft refresh deadline (refreshTTL after
//     load); past it the loader is re-attempted on Get. When the loader
//     fails (DB outage) an entry still inside its valid_until window is
//     served as an approved degraded read and marked degraded so the next
//     Get retries (self-heals on recovery). Expired entries are never served.

import (
	"context"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// LoadedSnapshot is what a loader returns: the active published snapshot
// plus its parsed profiles.
type LoadedSnapshot struct {
	SnapshotID      int64
	ProviderID      int64
	ProviderKey     string
	ProviderType    string
	EndpointRef     string
	SnapshotVersion int
	ContractVersion string
	Profiles        []map[string]any
	GeneratedAt     time.Time
	ValidUntil      time.Time
}

// SnapshotLoader loads the active published snapshot for a provider.
type SnapshotLoader func(ctx context.Context, providerID int64) (*LoadedSnapshot, error)

// CacheStats exposes counters for monitoring/tests.
type CacheStats struct {
	Loads        int64 // singleflight-coalesced loader invocations
	LoadErrors   int64
	DegradedHits int64 // served from cache while the loader failed
}

// SnapshotCache caches active published snapshots per provider.
type SnapshotCache struct {
	loader     SnapshotLoader
	refreshTTL time.Duration
	now        func() time.Time

	mu      sync.RWMutex
	entries map[int64]*cacheEntry
	sf      singleflight.Group

	statsMu sync.Mutex
	stats   CacheStats
}

type cacheEntry struct {
	snap     *LoadedSnapshot
	loaded   time.Time
	softEnd  time.Time // refresh deadline; before it the entry serves as-is
	degraded bool      // last loader attempt failed; entry served within window
}

// NewSnapshotCache builds a cache over the loader. refreshTTL is the soft
// refresh cadence: after it passes, Get re-attempts the loader (coalesced)
// while an unexpired entry keeps serving on failure (approved degraded
// read, GWT#9).
func NewSnapshotCache(loader SnapshotLoader, refreshTTL time.Duration) *SnapshotCache {
	return &SnapshotCache{
		loader:     loader,
		refreshTTL: refreshTTL,
		now:        time.Now,
		entries:    map[int64]*cacheEntry{},
	}
}

// Get returns the active snapshot for the provider: fresh entries from
// memory, first loads coalesced, degraded (unexpired) entries served when
// the loader fails and retried on the next call.
func (c *SnapshotCache) Get(ctx context.Context, providerID int64) (*LoadedSnapshot, error) {
	now := c.now()
	c.mu.RLock()
	e, ok := c.entries[providerID]
	// fast path: inside both the validity window and the soft refresh
	// window, and not marked degraded from a failed refresh attempt
	fresh := ok && e.snap.ValidUntil.After(now) && !e.degraded && now.Before(e.softEnd)
	c.mu.RUnlock()
	if fresh {
		return e.snap, nil
	}

	v, err, _ := c.sf.Do(strconv.FormatInt(providerID, 10), func() (any, error) {
		c.statsMu.Lock()
		c.stats.Loads++
		c.statsMu.Unlock()
		loaded, lerr := c.loader(ctx, providerID)
		c.statsMu.Lock()
		if lerr != nil {
			c.stats.LoadErrors++
		}
		c.statsMu.Unlock()
		if lerr != nil {
			// degraded read: only within the approved validity window
			c.mu.Lock()
			old, had := c.entries[providerID]
			if had && old.snap.ValidUntil.After(c.now()) {
				old.degraded = true
				c.mu.Unlock()
				c.statsMu.Lock()
				c.stats.DegradedHits++
				c.statsMu.Unlock()
				return old.snap, nil
			}
			c.mu.Unlock()
			return nil, lerr
		}
		c.mu.Lock()
		now := c.now()
		c.entries[providerID] = &cacheEntry{snap: loaded, loaded: now, softEnd: now.Add(c.refreshTTL)}
		c.mu.Unlock()
		return loaded, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*LoadedSnapshot), nil
}

// Loaded reports how many distinct providers are currently cached.
func (c *SnapshotCache) Loaded() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Stats returns a copy of the counters.
func (c *SnapshotCache) Stats() CacheStats {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	return c.stats
}
