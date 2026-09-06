package handler

import "sync"

// tenantCache holds one lazily-built value per workspace.
//
// It replaces the Set/get singleton pairs that used to sit on shared behind a
// mutex each. The shape is the same — build once, read many, replace on save —
// with a workspace id in front of it.
//
// ⛔ The key is "" in single-tenant mode, not the literal "default". That is
// deliberate: one entry, built the first time it is asked for, replaced by
// SetX exactly as before. A single-tenant instance therefore behaves
// identically and the map never holds more than one entry.
//
// present is separate from entries because nil is a MEANINGFUL value here: a
// workspace with no Stripe credentials caches a nil *stripe.Client, and that
// must not be mistaken for "not built yet" or every request would rebuild it.
type tenantCache[T any] struct {
	mu      sync.RWMutex
	entries map[string]T
	present map[string]bool
}

func newTenantCache[T any]() *tenantCache[T] {
	return &tenantCache[T]{entries: map[string]T{}, present: map[string]bool{}}
}

// get returns the cached value for key, building it with build if absent.
//
// build runs WITHOUT the lock held, because every builder here reads
// server_settings — holding the write lock across a database round trip would
// serialise every tenant behind whichever one was slowest to build. The cost is
// that two concurrent first-requests for the same workspace may both build; the
// first store wins and the loser's value is discarded. Clients are stateless
// value wrappers, so that is waste and not a correctness problem.
func (c *tenantCache[T]) get(key string, build func() T) T {
	c.mu.RLock()
	if c.present[key] {
		v := c.entries[key]
		c.mu.RUnlock()
		return v
	}
	c.mu.RUnlock()

	built := build()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.present[key] {
		return c.entries[key] // another goroutine got there first; keep one
	}
	c.entries[key] = built
	c.present[key] = true
	return built
}

// set replaces the value for key. The settings-save handlers call it after a
// successful write, which is both the invalidation and the rebuild.
func (c *tenantCache[T]) set(key string, v T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = v
	c.present[key] = true
}

// invalidate drops the value for key, so the next get rebuilds it from the
// database. For a save path that does not already hold the built client.
func (c *tenantCache[T]) invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
	delete(c.present, key)
}

// size reports how many workspaces have a cached value. For tests.
func (c *tenantCache[T]) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.present)
}

// cacheKey is the key every per-workspace cache is read and written under.
//
// ⛔ "" in single-tenant mode rather than the workspace id, so that the map holds
// exactly one entry and a single-tenant instance cannot be made to build a second
// one by anything that calls ForWorkspace.
func (h *Handler) cacheKey() string {
	if !h.multiTenant {
		return ""
	}
	return h.Workspace().ID
}
