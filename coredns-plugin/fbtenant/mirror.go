package fbtenant

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// mirrorEntry tracks one learned IP in one set.
type mirrorEntry struct {
	deadline time.Time // when the element expires in nftables
	timeout  time.Duration
}

// setMirror is the plugin-side membership cache of one nftables timeout set.
// nftables stays the source of truth; the mirror is rebuilt on resync.
type setMirror struct {
	name    string
	entries map[string]mirrorEntry
}

// AddResult reports what a single AddIfAbsent batch did.
type AddResult struct {
	Added    int // new or refreshed elements written to nft
	Skipped  int // duplicates skipped without touching nft
	Evicted  int // oldest entries evicted due to max_set_size
	WriteErr error
	EvictErr error
}

// mirrorManager owns all set mirrors of all tenants and routes every nftables
// mutation through NFTOps.
type mirrorManager struct {
	mu    sync.Mutex
	nft   NFTOps
	table string

	refreshPct  int           // refresh_on_remain_pct from state
	maxSetSize  int           // max_set_size from state (0 = unlimited)
	ttlMin      time.Duration // clamp floor
	ttlMax      time.Duration // clamp ceiling
	sets        map[string]*setMirror
	lastResync  time.Time
	resyncCount int64
}

func newMirrorManager(nft NFTOps, table string, st *State) *mirrorManager {
	m := &mirrorManager{
		nft:   nft,
		table: table,
		sets:  map[string]*setMirror{},
	}
	if st != nil {
		m.refreshPct = st.RefreshOnRemainPct
		m.maxSetSize = st.MaxSetSize
		m.ttlMin = time.Duration(st.TTLMinSec) * time.Second
		m.ttlMax = time.Duration(st.TTLMaxSec) * time.Second
		for _, t := range st.Tenants {
			for _, kind := range []string{SetIncDNS, SetExcDNS} {
				if name := t.Sets[kind]; name != "" {
					m.ensureSet(name)
				}
			}
		}
	}
	return m
}

func (m *mirrorManager) ensureSet(name string) *setMirror {
	sm, ok := m.sets[name]
	if !ok {
		sm = &setMirror{name: name, entries: map[string]mirrorEntry{}}
		m.sets[name] = sm
	}
	return sm
}

// updateKnobs refreshes state-derived parameters after a state reload.
func (m *mirrorManager) updateKnobs(st *State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshPct = st.RefreshOnRemainPct
	m.maxSetSize = st.MaxSetSize
	m.ttlMin = time.Duration(st.TTLMinSec) * time.Second
	m.ttlMax = time.Duration(st.TTLMaxSec) * time.Second
	// Drop mirrors of sets that disappeared from the state.
	keep := map[string]bool{}
	for _, t := range st.Tenants {
		for _, kind := range []string{SetIncDNS, SetExcDNS} {
			if name := t.Sets[kind]; name != "" {
				keep[name] = true
			}
		}
	}
	for name := range m.sets {
		if !keep[name] {
			delete(m.sets, name)
		}
	}
	for _, t := range st.Tenants {
		for _, kind := range []string{SetIncDNS, SetExcDNS} {
			if name := t.Sets[kind]; name != "" {
				m.ensureSet(name)
			}
		}
	}
}

// AddIfAbsent adds learned IPs to a set without re-adding live elements.
// CONTRACTS §3:
//   - present and not expired -> no nft call, counted as duplicate;
//   - refresh_on_remain_pct > 0 and remaining < pct% of timeout -> one update,
//     merged into the same batch (one AddElements per DNS response);
//   - otherwise the IP joins the add batch.
//
// maxSetSize overflow evicts the oldest entries (by deadline) first.
func (m *mirrorManager) AddIfAbsent(setName string, ips []string, timeout time.Duration, now time.Time) AddResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := AddResult{}
	if len(ips) == 0 || timeout <= 0 {
		return res
	}
	sm := m.ensureSet(setName)

	batch := map[string]time.Duration{}
	for _, ip := range ips {
		e, ok := sm.entries[ip]
		if ok && now.Before(e.deadline) {
			if m.refreshPct > 0 && e.timeout > 0 {
				remaining := e.deadline.Sub(now)
				if time.Duration(m.refreshPct)*e.timeout/100 > remaining {
					// Refresh: one update, merged into the batch; not a
					// duplicate skip since nft is touched.
					batch[ip] = timeout
					continue
				}
			}
			res.Skipped++
			continue
		}
		batch[ip] = timeout
	}
	if len(batch) == 0 {
		return res
	}

	res.Evicted, res.EvictErr = m.evictOverflowLocked(sm, batch)
	// On evict error still try the add; resync will reconcile.
	added, err := m.applyBatch(setName, sm, batch, now)
	res.Added, res.WriteErr = added, err
	return res
}

// Refresh unconditionally re-adds ips with a fresh timeout, extending their
// deadlines (keeper keep-alive path; CONTRACTS §3). Elements no longer present
// in the answer are NOT removed: several domains may share CDN IPs.
func (m *mirrorManager) Refresh(setName string, ips []string, timeout time.Duration, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(ips) == 0 || timeout <= 0 {
		return nil
	}
	sm := m.ensureSet(setName)
	batch := map[string]time.Duration{}
	for _, ip := range ips {
		batch[ip] = timeout
	}
	// Overflow eviction is best-effort here; the add below still runs and a
	// resync reconciles.
	_, _ = m.evictOverflowLocked(sm, batch)
	_, err := m.applyBatch(setName, sm, batch, now)
	return err
}

// applyBatch writes the batch to nft and updates the mirror on success.
func (m *mirrorManager) applyBatch(setName string, sm *setMirror, batch map[string]time.Duration, now time.Time) (int, error) {
	if err := m.nft.AddElements(m.table, setName, batch); err != nil {
		return 0, fmt.Errorf("add to %s: %w", setName, err)
	}
	for ip, t := range batch {
		sm.entries[ip] = mirrorEntry{deadline: now.Add(t), timeout: t}
	}
	return len(batch), nil
}

// evictOverflowLocked enforces maxSetSize: when adding batch would overflow,
// the oldest entries (by deadline) not in the batch are deleted first.
func (m *mirrorManager) evictOverflowLocked(sm *setMirror, batch map[string]time.Duration) (int, error) {
	if m.maxSetSize <= 0 {
		return 0, nil
	}
	over := len(sm.entries) + len(batch) - m.maxSetSize
	if over <= 0 {
		return 0, nil
	}
	candidates := make([]string, 0, len(sm.entries))
	for ip := range sm.entries {
		if _, inBatch := batch[ip]; !inBatch {
			candidates = append(candidates, ip)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return sm.entries[candidates[i]].deadline.Before(sm.entries[candidates[j]].deadline)
	})
	if over > len(candidates) {
		over = len(candidates)
	}
	victims := candidates[:over]
	if err := m.nft.DeleteElements(m.table, sm.name, victims); err != nil {
		return 0, fmt.Errorf("evict from %s: %w", sm.name, err)
	}
	for _, ip := range victims {
		delete(sm.entries, ip)
	}
	return len(victims), nil
}

// MinDeadline returns the earliest deadline among ips known to the mirror
// and still live at now. Callers use it to align a keeper record with the
// actual element lifetimes after duplicate-skipped learns and resyncs.
func (m *mirrorManager) MinDeadline(setName string, ips []string, now time.Time) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sm, ok := m.sets[setName]
	if !ok {
		return time.Time{}, false
	}
	var best time.Time
	found := false
	for _, ip := range ips {
		e, ok := sm.entries[ip]
		if !ok || !now.Before(e.deadline) {
			continue
		}
		if !found || e.deadline.Before(best) {
			best, found = e.deadline, true
		}
	}
	return best, found
}

// Resync dumps every known set from nftables and rebuilds the mirrors:
// expired entries disappear, unknown entries are picked up.
func (m *mirrorManager) Resync(now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for name, sm := range m.sets {
		dump, err := m.nft.ListSet(m.table, name)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fresh := map[string]mirrorEntry{}
		for ip, elem := range dump {
			deadline := now.Add(elem.Remaining)
			timeout := elem.Timeout
			if timeout <= 0 {
				timeout = elem.Remaining
			}
			fresh[ip] = mirrorEntry{deadline: deadline, timeout: timeout}
		}
		sm.entries = fresh
	}
	if firstErr == nil {
		m.lastResync = now
		m.resyncCount++
	}
	return firstErr
}

// SetSize returns the mirrored element count of one set.
func (m *mirrorManager) SetSize(setName string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sm, ok := m.sets[setName]; ok {
		return len(sm.entries)
	}
	return 0
}

// SetNames returns the currently tracked set names (sorted).
func (m *mirrorManager) SetNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.sets))
	for n := range m.sets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ResyncStats returns the last successful resync time and total count.
func (m *mirrorManager) ResyncStats() (time.Time, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastResync, m.resyncCount
}
