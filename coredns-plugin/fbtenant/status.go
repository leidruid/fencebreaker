package fbtenant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// tenantStats holds per-tenant counters written to plugin_status.json (§4).
type tenantStats struct {
	queries           atomic.Int64
	cacheHits         atomic.Int64
	learned           atomic.Int64
	duplicatesSkipped atomic.Int64
	upstreamErrors    atomic.Int64

	errMu   sync.Mutex
	lastErr string
}

func (s *tenantStats) setLastError(msg string) {
	s.errMu.Lock()
	s.lastErr = msg
	s.errMu.Unlock()
}

func (s *tenantStats) lastError() string {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.lastErr
}

// tenantStatus is the per-tenant section of plugin_status.json.
type tenantStatus struct {
	Queries           int64                `json:"queries"`
	CacheHits         int64                `json:"cache_hits"`
	Learned           int64                `json:"learned"`
	DuplicatesSkipped int64                `json:"duplicates_skipped"`
	UpstreamErrors    int64                `json:"upstream_errors"`
	SetSizes          map[string]int       `json:"set_sizes"`
	Upstreams         []UpstreamHealthInfo `json:"upstreams,omitempty"`
	LastError         string               `json:"last_error"`
	// Keeper is present only for dns_keepalive tenants.
	Keeper *keeperStatus `json:"keeper,omitempty"`
}

// keeperStatus reports the domain keep-alive state (CONTRACTS §3).
type keeperStatus struct {
	Domains     int    `json:"domains"`
	Refreshes   int64  `json:"refreshes"`
	Errors      int64  `json:"errors"`
	LastRefresh string `json:"last_refresh,omitempty"`
}

// statusDoc is the on-disk shape of plugin_status.json.
type statusDoc struct {
	Updated    string                  `json:"updated"`
	Generation int64                   `json:"generation"`
	LastResync string                  `json:"last_resync"`
	Resyncs    int64                   `json:"resyncs"`
	Tenants    map[string]tenantStatus `json:"tenants"`
}

// tenantStatuses collects per-tenant counters and mirror set sizes.
func (f *FBTenant) tenantStatuses() map[string]tenantStatus {
	out := map[string]tenantStatus{}
	cfg := f.currentCfg()
	if cfg == nil {
		return out
	}
	for _, t := range cfg.tenants {
		st := tenantStatus{
			Queries:           t.stats.queries.Load(),
			CacheHits:         t.stats.cacheHits.Load(),
			Learned:           t.stats.learned.Load(),
			DuplicatesSkipped: t.stats.duplicatesSkipped.Load(),
			UpstreamErrors:    t.stats.upstreamErrors.Load(),
			LastError:         t.stats.lastError(),
			SetSizes:          map[string]int{},
		}
		if hv, ok := f.client.(healthViewer); ok && len(t.upstreams) > 0 {
			st.Upstreams = hv.UpstreamHealth(t.upstreams)
		}
		for _, kind := range []string{SetIncDNS, SetExcDNS} {
			if name := t.setName(kind); name != "" {
				st.SetSizes[kind] = f.mirrorSize(name)
			}
		}
		if t.keepalive {
			if k := f.keeperIfAny(t.raw.ID); k != nil {
				domains, refreshes, errors, last := k.stats()
				ks := keeperStatus{Domains: domains, Refreshes: refreshes, Errors: errors}
				if !last.IsZero() {
					ks.LastRefresh = last.UTC().Format(time.RFC3339)
				}
				st.Keeper = &ks
			} else {
				st.Keeper = &keeperStatus{}
			}
		}
		out[t.raw.ID] = st
	}
	return out
}

// statusPathFor returns the plugin_status.json path next to the state file.
func statusPathFor(stateFile string) string {
	return filepath.Join(filepath.Dir(stateFile), "plugin_status.json")
}

// writeStatus atomically writes plugin_status.json (tmp + rename).
func (f *FBTenant) writeStatus(now time.Time) error {
	doc := statusDoc{
		Updated:    now.UTC().Format(time.RFC3339),
		Tenants:    f.tenantStatuses(),
		Generation: 0,
	}
	if cfg := f.currentCfg(); cfg != nil {
		doc.Generation = cfg.generation
	}
	if m := f.getMirrors(); m != nil {
		last, count := m.ResyncStats()
		doc.Resyncs = count
		if !last.IsZero() {
			doc.LastResync = last.UTC().Format(time.RFC3339)
		}
	}

	path := f.statusFile
	if path == "" {
		if f.stateFile == "" {
			return nil
		}
		path = statusPathFor(f.stateFile)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".plugin_status-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
