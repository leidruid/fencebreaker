package fbtenant

import (
	"strconv"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// maxCacheEntries is the per-tenant cache limit.
const maxCacheEntries = 10000

type cacheEntry struct {
	msg     *dns.Msg
	expires time.Time
}

// tenantCache is a small per-tenant positive-answer cache keyed by
// "qname|qtype". Eviction is time-based: expired entries are purged lazily
// and, when the map is full, the earliest-expiring entry is dropped.
type tenantCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	max     int
}

func newTenantCache(max int) *tenantCache {
	if max <= 0 {
		max = maxCacheEntries
	}
	return &tenantCache{entries: map[string]cacheEntry{}, max: max}
}

func cacheKey(qname string, qtype uint16) string {
	return qname + "|" + strconv.Itoa(int(qtype))
}

// get returns a copy of the cached message, or nil.
func (c *tenantCache) get(key string, now time.Time) *dns.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil
	}
	if !now.Before(e.expires) {
		delete(c.entries, key)
		return nil
	}
	return e.msg.Copy()
}

// put stores a positive answer with the given TTL.
func (c *tenantCache) put(key string, msg *dns.Msg, ttl time.Duration, now time.Time) {
	if msg == nil || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		c.purgeExpiredLocked(now)
	}
	if len(c.entries) >= c.max {
		// Evict the earliest-expiring entry.
		var oldestKey string
		var oldest time.Time
		first := true
		for k, e := range c.entries {
			if first || e.expires.Before(oldest) {
				oldestKey, oldest, first = k, e.expires, false
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = cacheEntry{msg: msg.Copy(), expires: now.Add(ttl)}
}

func (c *tenantCache) purgeExpiredLocked(now time.Time) {
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
}

func (c *tenantCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
