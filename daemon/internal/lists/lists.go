// Package lists manages URL-subscribed resource lists: download, parse
// (subnets and domains mixed, auto-detected per line), cache on disk and
// expose for nftgen/dnsgen.
package lists

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/leidruid/fencebreaker/daemon/internal/httpx"
	"github.com/leidruid/fencebreaker/daemon/internal/model"
	"github.com/leidruid/fencebreaker/daemon/internal/nftgen"
)

// MaxListBytes caps one list download.
const MaxListBytes = 32 << 20

// Entry is the runtime state of one list.
type Entry struct {
	Def      *model.ListDef
	Parsed   *nftgen.ParsedList
	Updated  time.Time
	State    string // ok | error | pending
	Error    string
}

// Manager keeps list caches fresh.
type Manager struct {
	Dir     string
	Client  *httpx.Client
	Now     func() time.Time

	mu      sync.Mutex
	entries map[string]*Entry
}

// New creates a manager storing raw downloads in dir.
func New(dir string) *Manager {
	return &Manager{
		Dir:     dir,
		Client:  httpx.New(MaxListBytes),
		Now:     time.Now,
		entries: map[string]*Entry{},
	}
}

// LoadAll parses cached downloads for all list definitions (no network).
func (m *Manager) LoadAll(defs []*model.ListDef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	os.MkdirAll(m.Dir, 0o755)
	for _, def := range defs {
		e := &Entry{Def: def, State: "pending", Parsed: &nftgen.ParsedList{}}
		if def.URL == "" {
			// Manual list: content comes from the UCI content option,
			// no download and no disk cache involved.
			e.Parsed = Parse([]byte(def.Content))
			e.State = "ok"
			e.Updated = m.Now()
			m.entries[def.Name] = e
			continue
		}
		if raw, err := os.ReadFile(m.rawPath(def.Name)); err == nil {
			e.Parsed = Parse(raw)
			e.State = "ok"
			if fi, err := os.Stat(m.rawPath(def.Name)); err == nil {
				e.Updated = fi.ModTime()
			}
		}
		m.entries[def.Name] = e
	}
}

// Refresh downloads one list if its refresh interval has elapsed.
func (m *Manager) Refresh(ctx context.Context, def *model.ListDef) error {
	m.mu.Lock()
	e := m.entries[def.Name]
	m.mu.Unlock()
	if e == nil {
		e = &Entry{Def: def, State: "pending", Parsed: &nftgen.ParsedList{}}
		m.mu.Lock()
		m.entries[def.Name] = e
		m.mu.Unlock()
	}
	if def.URL == "" {
		m.mu.Lock()
		e.Parsed = Parse([]byte(def.Content))
		e.Updated = m.Now()
		e.State = "ok"
		e.Error = ""
		m.mu.Unlock()
		return nil
	}
	data, err := m.Client.Get(ctx, def.URL)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		e.State = "error"
		e.Error = err.Error()
		return err
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		e.State = "error"
		e.Error = err.Error()
		return err
	}
	tmp := m.rawPath(def.Name) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		e.State = "error"
		e.Error = err.Error()
		return err
	}
	if err := os.Rename(tmp, m.rawPath(def.Name)); err != nil {
		e.State = "error"
		e.Error = err.Error()
		return err
	}
	e.Parsed = Parse(data)
	e.Updated = m.Now()
	e.State = "ok"
	e.Error = ""
	return nil
}

// DueForRefresh reports whether the list should be downloaded now. Manual
// lists (no url) are never downloaded and are therefore never due.
func (m *Manager) DueForRefresh(def *model.ListDef) bool {
	if def.URL == "" {
		return false
	}
	m.mu.Lock()
	e := m.entries[def.Name]
	m.mu.Unlock()
	if e == nil || e.State == "pending" {
		return true
	}
	if def.RefreshH <= 0 {
		return false
	}
	return m.Now().Sub(e.Updated) > time.Duration(def.RefreshH)*time.Hour
}

// Snapshot returns the parsed content of all known lists keyed by name.
func (m *Manager) Snapshot() map[string]*nftgen.ParsedList {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]*nftgen.ParsedList{}
	for name, e := range m.entries {
		out[name] = e.Parsed
	}
	return out
}

// Statuses returns list states for status.json.
func (m *Manager) Statuses() []EntryStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []EntryStatus
	for _, name := range sortedKeys(m.entries) {
		e := m.entries[name]
		out = append(out, EntryStatus{
			Name: name, State: e.State, Updated: e.Updated,
			Nets: len(e.Parsed.Nets), Domains: len(e.Parsed.Domains), Error: e.Error,
		})
	}
	return out
}

// EntryStatus is the status view of one list.
type EntryStatus struct {
	Name    string    `json:"name"`
	State   string    `json:"state"`
	Updated time.Time `json:"updated"`
	Nets    int       `json:"nets"`
	Domains int       `json:"domains"`
	Error   string    `json:"error"`
}

func (m *Manager) rawPath(name string) string {
	return filepath.Join(m.Dir, name+".txt")
}

func sortedKeys(m map[string]*Entry) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

var domainLabel = func(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return s[0] != '-' && s[len(s)-1] != '-'
}

// Parse classifies each line: CIDR, bare IP (→ /32) or domain. Comments and
// blanks are skipped; malformed lines are ignored.
func Parse(data []byte) *nftgen.ParsedList {
	out := &nftgen.ParsedList{}
	seenN := map[string]bool{}
	seenD := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		line = strings.TrimSuffix(line, ",")
		if _, n, err := net.ParseCIDR(line); err == nil {
			c := n.String()
			if !seenN[c] {
				seenN[c] = true
				out.Nets = append(out.Nets, c)
			}
			continue
		}
		if ip := net.ParseIP(line); ip != nil {
			c := ipToCIDR(ip)
			if !seenN[c] {
				seenN[c] = true
				out.Nets = append(out.Nets, c)
			}
			continue
		}
		if d := normalizeDomain(line); d != "" {
			if !seenD[d] {
				seenD[d] = true
				out.Domains = append(out.Domains, d)
			}
			continue
		}
	}
	return out
}

func ipToCIDR(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String() + "/32"
	}
	return ip.String() + "/128"
}

func normalizeDomain(s string) string {
	s = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
	if !strings.Contains(s, ".") {
		return ""
	}
	labels := strings.Split(s, ".")
	if labels[0] == "" || labels[len(labels)-1] == "" {
		return ""
	}
	for _, l := range labels {
		if !domainLabel(l) {
			return ""
		}
	}
	return s
}
