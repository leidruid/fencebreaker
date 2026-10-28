// Package dnsgen builds the dns_state.json consumed by the fbtenant CoreDNS
// plugin (see docs/CONTRACTS.md §2) and manages the generation counter.
package dnsgen

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/leidruid/fencebreaker/daemon/internal/model"
	"github.com/leidruid/fencebreaker/daemon/internal/nftgen"
)

// State is the whole dns_state.json document.
type State struct {
	Version            int            `json:"version"`
	Generation         uint64         `json:"generation"`
	ListenPort         int            `json:"listen_port"`
	ResyncIntervalSec  int            `json:"resync_interval_sec"`
	TTLMinSec          int            `json:"ttl_min_sec"`
	TTLMaxSec          int            `json:"ttl_max_sec"`
	RefreshOnRemainPct int            `json:"refresh_on_remain_pct"`
	MaxSetSize         int            `json:"max_set_size"`
	DefaultUpstreams   []string       `json:"default_upstreams"`
	NftTable           string         `json:"nft_table"`
	Tenants            []*TenantState `json:"tenants"`
}

// TenantState is the per-tenant part of the state.
type TenantState struct {
	ID             string   `json:"id"`
	Mark           uint32   `json:"mark"`
	SrcNets        []string `json:"src_nets"`
	HostsFull      []string `json:"hosts_full"`
	Sets           Sets     `json:"sets"`
	LearningMode   string   `json:"learning_mode"`
	IncludeDomains []string `json:"include_domains"`
	ExcludeDomains []string `json:"exclude_domains"`
	// Upstreams are the raw dns_upstream entries; the fbtenant plugin parses
	// the 11 supported address forms itself.
	Upstreams      []string  `json:"upstreams"`
	UpstreamPolicy string    `json:"upstream_policy"`
	DNSViaTunnel   bool      `json:"dns_via_tunnel"`
	DNSKeepAlive   bool      `json:"dns_keepalive"`
	Records        []Record  `json:"records"`
	Forwards       []Forward `json:"forwards"`
}

// Sets names the nft sets the plugin may update.
type Sets struct {
	IncDNS string `json:"inc_dns"`
	ExcDNS string `json:"exc_dns"`
}

// Record is a static A record.
type Record struct {
	Domain string   `json:"domain"`
	IPs    []string `json:"ips"`
}

// Forward is a per-domain forward zone.
type Forward struct {
	Domain  string   `json:"domain"`
	Servers []string `json:"servers"`
}

// Build assembles the state from the model. srcNets maps tenant ID to
// resolved subnets; parsed lists contribute their domains.
func Build(cfg *model.Config, srcNets map[string][]string, parsed map[string]*nftgen.ParsedList) *State {
	st := &State{
		Version:            1,
		ListenPort:         cfg.Global.CoreDNSPort,
		ResyncIntervalSec:  cfg.Global.ResyncInterval,
		TTLMinSec:          cfg.Global.TTLMin,
		TTLMaxSec:          cfg.Global.TTLMax,
		RefreshOnRemainPct: cfg.Global.DNSRefreshOnRemainPct,
		MaxSetSize:         cfg.Global.MaxSetSize,
		DefaultUpstreams:   []string{"127.0.0.1:53"},
		NftTable:           "fencebreaker",
	}
	for _, t := range cfg.EnabledTenants() {
		incD := append([]string{}, t.IncludeDomains...)
		excD := append([]string{}, t.ExcludeDomains...)
		for _, name := range t.IncludeLists {
			if l := parsed[name]; l != nil {
				incD = append(incD, l.Domains...)
			}
		}
		for _, name := range t.ExcludeLists {
			if l := parsed[name]; l != nil {
				excD = append(excD, l.Domains...)
			}
		}
		ts := &TenantState{
			ID:             t.ID,
			Mark:           t.Mark(),
			SrcNets:        srcNets[t.ID],
			HostsFull:      canonIPs(t.FullHosts),
			Sets:           Sets{IncDNS: t.Sets().IncDNS, ExcDNS: t.Sets().ExcDNS},
			LearningMode:   t.RoutingMode,
			IncludeDomains: incD,
			ExcludeDomains: excD,
			Upstreams:      append([]string{}, t.DNSUpstream...),
			UpstreamPolicy: upstreamPolicy(t.UpstreamPolicy),
			DNSViaTunnel:   t.DNSViaTunnel,
			DNSKeepAlive:   t.DNSKeepAlive,
		}
		for _, r := range t.DNSRules {
			switch r.Type {
			case "record":
				ts.Records = append(ts.Records, Record{Domain: fq(r.Domain), IPs: canonIPs(r.Targets)})
			case "forward":
				ts.Forwards = append(ts.Forwards, Forward{Domain: fq(r.Domain), Servers: hostPort(r.Targets)})
			}
		}
		st.Tenants = append(st.Tenants, ts)
	}
	return st
}

// Write saves the state atomically and returns whether the tenant→set-name
// mapping changed compared to prev (used to bump the generation).
func Write(path string, st *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MappingChanged reports whether the tenant→(inc,exc) set mapping differs.
func MappingChanged(st *State, prevSets map[string][2]string) bool {
	if len(prevSets) != len(st.Tenants) {
		return true
	}
	for _, t := range st.Tenants {
		if prev, ok := prevSets[t.ID]; !ok || prev != [2]string{t.Sets.IncDNS, t.Sets.ExcDNS} {
			return true
		}
	}
	return false
}

func fq(d string) string {
	if len(d) > 0 && d[len(d)-1] == '.' {
		return d
	}
	return d + "."
}

// upstreamPolicy normalizes the tenant's upstream selection policy; an empty
// value (option absent) means the historical default, sequential.
func upstreamPolicy(p string) string {
	if p == "" {
		return "sequential"
	}
	return p
}

func hostPort(in []string) []string {
	var out []string
	for _, s := range in {
		if _, _, err := net.SplitHostPort(s); err == nil {
			out = append(out, s)
		} else if net.ParseIP(s) != nil {
			out = append(out, s+":53")
		}
	}
	return out
}

func canonIPs(in []string) []string {
	var out []string
	for _, s := range in {
		if ip := net.ParseIP(s); ip != nil {
			if v4 := ip.To4(); v4 != nil {
				out = append(out, v4.String())
			}
		}
	}
	return out
}

// String renders the state for logs.
func (s *State) String() string {
	return fmt.Sprintf("state(gen=%d, tenants=%d)", s.Generation, len(s.Tenants))
}
