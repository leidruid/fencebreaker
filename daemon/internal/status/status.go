// Package status assembles and atomically writes /var/run/fencebreaker/
// status.json (docs/CONTRACTS.md §5), merging the counters published by the
// fbtenant CoreDNS plugin.
package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/leidruid/fencebreaker/daemon/internal/model"
)

// Document is the whole status.json.
type Document struct {
	Version int       `json:"version"`
	Updated time.Time `json:"updated"`
	Daemon  Daemon    `json:"daemon"`
	Tenants []Tenant  `json:"tenants"`
	Lists   []List    `json:"lists"`
	Geodata Geodata   `json:"geodata"`
}

// Daemon is the daemon's own state.
type Daemon struct {
	Generation uint64    `json:"generation"`
	UptimeSec  int64     `json:"uptime_sec"`
	LastApply  time.Time `json:"last_apply"`
	LastError  string    `json:"last_error"`
}

// Tenant is the status view of one tenant.
type Tenant struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Enabled     bool        `json:"enabled"`
	Table       int         `json:"table"`
	Mark        uint32      `json:"mark"`
	Egress      Egress      `json:"egress"`
	RoutingMode string      `json:"routing_mode"`
	Fallback    string      `json:"fallback"`
	Sets        SetSizes    `json:"sets"`
	DNS         DNSCounters `json:"dns"`
	Errors      []string    `json:"errors"`
}

// Egress describes the tenant channel state.
type Egress struct {
	Mode   string `json:"mode"`
	Target string `json:"target"`
	State  string `json:"state"`
}

// SetSizes reports element counts of the tenant's nft sets.
type SetSizes struct {
	IncStatic int `json:"inc_static"`
	IncGeoIP  int `json:"inc_geoip"`
	IncDNS    int `json:"inc_dns"`
	ExcStatic int `json:"exc_static"`
	ExcGeoIP  int `json:"exc_geoip"`
	ExcDNS    int `json:"exc_dns"`
}

// DNSCounters comes from plugin_status.json.
type DNSCounters struct {
	Queries           int64 `json:"queries"`
	CacheHits         int64 `json:"cache_hits"`
	Learned           int64 `json:"learned"`
	DuplicatesSkipped int64 `json:"duplicates_skipped"`
	UpstreamErrors    int64 `json:"upstream_errors"`
	// Keeper mirrors the fbtenant keep-alive section; nil for tenants
	// without dns_keepalive.
	Keeper *KeeperStatus `json:"keeper,omitempty"`
}

// KeeperStatus is the keep-alive part of plugin_status.json.
type KeeperStatus struct {
	Domains     int    `json:"domains"`
	Refreshes   int64  `json:"refreshes"`
	Errors      int64  `json:"errors"`
	LastRefresh string `json:"last_refresh,omitempty"`
}

// List is the status of an URL-subscribed list.
type List struct {
	Name    string    `json:"name"`
	State   string    `json:"state"`
	Updated time.Time `json:"updated"`
	Nets    int       `json:"nets"`
	Domains int       `json:"domains"`
	Error   string    `json:"error"`
}

// Geodata is the state of the geoip database.
type Geodata struct {
	State   string    `json:"state"`
	Updated time.Time `json:"updated"`
	Entries int       `json:"entries"`
	Error   string    `json:"error"`
}

// PluginStatus mirrors plugin_status.json (subset we merge).
type PluginStatus struct {
	Updated time.Time `json:"updated"`
	Tenants map[string]struct {
		Queries           int64 `json:"queries"`
		CacheHits         int64 `json:"cache_hits"`
		Learned           int64 `json:"learned"`
		DuplicatesSkipped int64 `json:"duplicates_skipped"`
		UpstreamErrors    int64 `json:"upstream_errors"`
		SetSizes          struct {
			IncDNS int `json:"inc_dns"`
		} `json:"set_sizes"`
		LastError string        `json:"last_error"`
		Keeper    *KeeperStatus `json:"keeper"`
	} `json:"tenants"`
}

// Writer writes status documents atomically.
type Writer struct {
	Path string
}

// New creates a writer for path.
func New(path string) *Writer { return &Writer{Path: path} }

// Write assembles the document: cfg supplies tenants, setSizes and dns
// counters are filled from live data when available.
func (w *Writer) Write(doc *Document) error {
	if err := os.MkdirAll(filepath.Dir(w.Path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := w.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.Path)
}

// LoadPlugin reads plugin_status.json; a missing file is not an error.
func LoadPlugin(path string) (*PluginStatus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &PluginStatus{}, nil
		}
		return nil, err
	}
	var ps PluginStatus
	if err := json.Unmarshal(data, &ps); err != nil {
		return nil, err
	}
	return &ps, nil
}

// TenantsFrom builds tenant status entries from the model.
func TenantsFrom(cfg *model.Config) []Tenant {
	var out []Tenant
	for _, t := range cfg.Tenants {
		target := t.EgressInterface
		if t.EgressMode == model.EgressVLESS {
			target = t.VLESS.Server
		}
		st := Tenant{
			ID:          t.ID,
			Name:        t.Name,
			Enabled:     t.Enabled,
			Table:       t.TableID(),
			Mark:        t.Mark(),
			Egress:      Egress{Mode: string(t.EgressMode), Target: target, State: t.EgressState},
			RoutingMode: t.RoutingMode,
			Fallback:    t.Fallback,
		}
		if st.Egress.State == "" {
			st.Egress.State = "unknown"
		}
		if t.EgressError != "" {
			st.Errors = append(st.Errors, t.EgressError)
		}
		out = append(out, st)
	}
	return out
}
