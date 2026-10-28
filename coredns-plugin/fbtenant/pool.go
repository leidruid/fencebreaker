package fbtenant

import (
	"sort"
	"sync"
	"time"
)

// basePenalty is the "unused" window after the first failure; every next
// consecutive failure doubles it (CONTRACTS §2: 10s, 20s, 40s, ...).
const basePenalty = 10 * time.Second

// healthEntry is the failure state of one upstream (keyed by spec.Addr()).
type healthEntry struct {
	fails     int
	until     time.Time
	lastError string
	ok        uint64
	fail      uint64
}

// upstreamHealth tracks per-upstream availability across queries.
type upstreamHealth struct {
	mu      sync.Mutex
	entries map[string]*healthEntry
}

func newUpstreamHealth() *upstreamHealth {
	return &upstreamHealth{entries: map[string]*healthEntry{}}
}

func (h *upstreamHealth) entry(key string) *healthEntry {
	e, ok := h.entries[key]
	if !ok {
		e = &healthEntry{}
		h.entries[key] = e
	}
	return e
}

// markFail registers a failed exchange: the upstream becomes unusable for
// basePenalty doubled with each consecutive failure.
func (h *upstreamHealth) markFail(key, reason string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entry(key)
	e.fails++
	e.fail++
	e.lastError = reason
	e.until = now.Add(basePenalty << uint(e.fails-1))
}

// markOk registers a successful exchange and clears the penalty.
func (h *upstreamHealth) markOk(key string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entry(key)
	e.ok++
	e.fails = 0
	e.until = time.Time{}
	e.lastError = ""
}

func (h *upstreamHealth) get(key string) healthEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return *h.entry(key)
}

// healthy reports whether the upstream may be used right now.
func (h *upstreamHealth) healthy(key string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.entry(key).until.After(now)
}

// UpstreamHealthInfo is the status view of one upstream.
type UpstreamHealthInfo struct {
	Addr      string    `json:"addr"`
	State     string    `json:"state"` // ok | penalty
	Fails     int       `json:"consecutive_failures"`
	PenaltyUp time.Time `json:"penalty_until,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Success   uint64    `json:"success"`
	Failures  uint64    `json:"failures"`
}

// snapshot returns the health view of the given addresses (in order).
func (h *upstreamHealth) snapshot(addrs []string, now time.Time) []UpstreamHealthInfo {
	out := make([]UpstreamHealthInfo, 0, len(addrs))
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range addrs {
		e := h.entry(a)
		st := "ok"
		if e.until.After(now) {
			st = "penalty"
		}
		out = append(out, UpstreamHealthInfo{
			Addr: a, State: st, Fails: e.fails, PenaltyUp: e.until,
			LastError: e.lastError, Success: e.ok, Failures: e.fail,
		})
	}
	return out
}

// orderForQuery ranks upstreams for one query: healthy ones first (shuffled
// for the random policy, configured order for sequential), then penalized
// ones by soonest expiry, so a fully penalized list still tries the server
// closest to recovery rather than failing outright.
func orderForQuery(specs []*UpstreamSpec, policy string, h *upstreamHealth, now time.Time, randFn func(n int) int) []*UpstreamSpec {
	healthy := make([]*UpstreamSpec, 0, len(specs))
	penalized := make([]*UpstreamSpec, 0, len(specs))
	for _, sp := range specs {
		if h.healthy(sp.Addr(), now) {
			healthy = append(healthy, sp)
		} else {
			penalized = append(penalized, sp)
		}
	}
	if NormalizePolicy(policy) == PolicyRandom && len(healthy) > 1 {
		shuffle(healthy, randFn)
	}
	sort.SliceStable(penalized, func(i, j int) bool {
		return h.get(penalized[i].Addr()).until.Before(h.get(penalized[j].Addr()).until)
	})
	return append(healthy, penalized...)
}

func shuffle(s []*UpstreamSpec, randFn func(n int) int) {
	if randFn == nil {
		return
	}
	for i := len(s) - 1; i > 0; i-- {
		j := randFn(i + 1)
		if j >= 0 && j <= i {
			s[i], s[j] = s[j], s[i]
		}
	}
}
