// Package model converts the UCI configuration into the daemon's runtime
// representation and validates it.
package model

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/leidruid/fencebreaker/daemon/internal/uci"
)

// EgressMode selects the tenant's outgoing channel.
type EgressMode string

const (
	EgressInterface EgressMode = "interface"
	EgressVLESS     EgressMode = "vless"
)

// VLESS holds the sing-box outbound parameters (TCP transport, TLS/REALITY).
type VLESS struct {
	UUID        string
	Server      string
	Port        int
	Flow        string
	Security    string // reality | tls | none
	SNI         string
	Fingerprint string
	PublicKey   string
	ShortID     string
}

// Tenant is one routing tenant.
type Tenant struct {
	ID       string
	Name     string
	Enabled  bool
	Priority int

	Index int // 1-based stable index within the applied config

	EgressMode      EgressMode
	EgressInterface string
	// VLESSURI is a vless:// share link; when set it supersedes the legacy
	// vless_* fields and is parsed into VLESS by the config loader.
	VLESSURI    string
	VLESS       VLESS
	Fallback    string // main | strict
	RoutingMode string // select | invert

	IncludeSubnets []string
	IncludeDomains []string
	IncludeGeoIP   []string
	IncludeLists   []string
	ExcludeSubnets []string
	ExcludeDomains []string
	ExcludeGeoIP   []string
	ExcludeLists   []string

	Networks      []string // uci network names and/or device names
	ExcludedHosts []string
	FullHosts     []string

	DNSHijack    bool
	DNSViaTunnel bool
	// DNSKeepAlive proactively re-resolves learned domains before their
	// nft elements expire (fbtenant keeper).
	DNSKeepAlive bool
	DNSUpstream  []string
	// UpstreamPolicy selects how the plugin picks a forward server:
	// "sequential" (list order, default) or "random".
	UpstreamPolicy string
	DNSRules       []DNSRule

	// Runtime state (not persisted): filled by the routing manager.
	EgressState string // up | down | strict
	EgressError string
}

// DNSRule is a per-tenant DNS override: a static record or a forward zone.
type DNSRule struct {
	Tenant  string
	Type    string // record | forward
	Domain  string
	Targets []string
}

// ListDef is an URL-subscribed resource list.
type ListDef struct {
	Name string
	URL  string
	// Content holds inline list lines for manual lists (url empty), one
	// entry per line; it is never written by the daemon, only edited via
	// LuCI/UCI.
	Content  string
	RefreshH int
}

// Global holds the global options.
type Global struct {
	Enabled               bool
	GeodataURL            string
	GeodataRefreshH       int
	ListsRefreshH         int
	CoreDNSPort           int
	TTLMin                int
	TTLMax                int
	DNSRefreshOnRemainPct int
	MaxSetSize            int
	ResyncInterval        int
}

// Config is the whole parsed configuration.
type Config struct {
	Global  Global
	Tenants []*Tenant
	Lists   []*ListDef
}

// MaxTenants is the number of fwmark values reserved for tenants.
const MaxTenants = 32

const (
	markBase = 0x1000
	// RuleBase is the base priority for tenant ip rules.
	RuleBase = 11000
)

// TableID returns the routing table number: 100 + index.
func (t *Tenant) TableID() int { return 100 + t.Index }

// Mark returns the exact fwmark value for the tenant: 0x1000 + (index-1).
func (t *Tenant) Mark() uint32 { return uint32(markBase + t.Index - 1) }

// RulePref returns the ip rule priority for the tenant.
func (t *Tenant) RulePref() int { return RuleBase + t.Index*10 }

// TunName returns the sing-box tun interface name for a VLESS tenant.
func (t *Tenant) TunName() string { return fmt.Sprintf("fb-tun%d", t.Index) }

// TunAddr returns the /30 tunnel address for a VLESS tenant.
func (t *Tenant) TunAddr() string { return fmt.Sprintf("10.231.%d.1/30", t.Index) }

// SetNames returns the nftables set names owned by the tenant.
type SetNames struct {
	IncStatic, IncGeoIP, IncDNS  string
	ExcStatic, ExcGeoIP, ExcDNS  string
	SrcNets, HostsExc, HostsFull string
}

// Sets returns the nftables set names for the tenant.
func (t *Tenant) Sets() SetNames {
	p := fmt.Sprintf("t%d_", t.Index)
	return SetNames{
		IncStatic: p + "inc_static",
		IncGeoIP:  p + "inc_geoip",
		IncDNS:    p + "inc_dns",
		ExcStatic: p + "exc_static",
		ExcGeoIP:  p + "exc_geoip",
		ExcDNS:    p + "exc_dns",
		SrcNets:   p + "src_nets",
		HostsExc:  p + "hosts_exc",
		HostsFull: p + "hosts_full",
	}
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// FromUCI builds the model from a parsed UCI config, assigning tenant
// indexes by the order of the tenant sections.
func FromUCI(cfg *uci.Config) (*Config, error) {
	c := &Config{}
	if g := cfg.Named("fencebreaker", "global"); g != nil {
		c.Global = Global{
			Enabled:               g.GetBool("enabled", true),
			GeodataURL:            g.Get("geodata_url", ""),
			GeodataRefreshH:       g.GetInt("geodata_refresh", 168),
			ListsRefreshH:         g.GetInt("lists_refresh", 24),
			CoreDNSPort:           g.GetInt("coredns_port", 1053),
			TTLMin:                g.GetInt("ttl_min", 300),
			TTLMax:                g.GetInt("ttl_max", 86400),
			DNSRefreshOnRemainPct: g.GetInt("dns_refresh_on_remain_pct", 0),
			MaxSetSize:            g.GetInt("max_set_size", 65536),
			ResyncInterval:        g.GetInt("resync_interval", 60),
		}
		if c.Global.GeodataURL == "" {
			c.Global.GeodataURL = "https://github.com/v2fly/geoip/releases/latest/download/geoip.dat"
		}
	}

	idx := 0
	for _, s := range cfg.SectionsOfType("tenant") {
		idx++
		t := &Tenant{
			ID:              s.Get("id", s.Name),
			Name:            s.Get("name", s.Name),
			Enabled:         s.GetBool("enabled", true),
			Priority:        s.GetInt("priority", 100),
			Index:           idx,
			EgressMode:      EgressMode(s.Get("egress_mode", string(EgressInterface))),
			EgressInterface: s.Get("egress_interface", ""),
			Fallback:        s.Get("fallback", "main"),
			RoutingMode:     s.Get("routing_mode", "select"),
			IncludeSubnets:  s.List("include_subnets"),
			IncludeDomains:  s.List("include_domains"),
			IncludeGeoIP:    s.List("include_geoip"),
			IncludeLists:    s.List("include_lists"),
			ExcludeSubnets:  s.List("exclude_subnets"),
			ExcludeDomains:  s.List("exclude_domains"),
			ExcludeGeoIP:    s.List("exclude_geoip"),
			ExcludeLists:    s.List("exclude_lists"),
			Networks:        s.List("networks"),
			ExcludedHosts:   s.List("excluded_hosts"),
			FullHosts:       s.List("full_hosts"),
			DNSHijack:       s.GetBool("dns_hijack", true),
			DNSViaTunnel:    s.GetBool("dns_via_tunnel", true),
			DNSKeepAlive:    s.GetBool("dns_keepalive", false),
			DNSUpstream:     s.List("dns_upstream"),
			UpstreamPolicy:  s.Get("upstream_policy", "sequential"),
		}
		v := VLESS{
			UUID:        s.Get("vless_uuid", ""),
			Server:      s.Get("vless_server", ""),
			Port:        s.GetInt("vless_port", 0),
			Flow:        s.Get("vless_flow", ""),
			Security:    s.Get("vless_security", "reality"),
			SNI:         s.Get("vless_sni", ""),
			Fingerprint: s.Get("vless_fp", "chrome"),
			PublicKey:   s.Get("vless_pbk", ""),
			ShortID:     s.Get("vless_sid", ""),
		}
		if t.EgressMode == EgressVLESS {
			t.VLESS = v
		}
		// A vless:// share link supersedes the legacy per-field options.
		t.VLESSURI = s.Get("vless_uri", "")
		if t.EgressMode == EgressVLESS && t.VLESSURI != "" {
			if parsed, err := ParseVLESSURI(t.VLESSURI); err == nil {
				t.VLESS = *parsed
			}
		}
		c.Tenants = append(c.Tenants, t)
	}

	for _, s := range cfg.SectionsOfType("dnsrule") {
		r := DNSRule{
			Tenant:  s.Get("tenant", ""),
			Type:    s.Get("type", "forward"),
			Domain:  s.Get("domain", ""),
			Targets: s.List("target"),
		}
		for i := range c.Tenants {
			if c.Tenants[i].ID == r.Tenant {
				c.Tenants[i].DNSRules = append(c.Tenants[i].DNSRules, r)
				break
			}
		}
	}

	for _, s := range cfg.SectionsOfType("list") {
		c.Lists = append(c.Lists, &ListDef{
			Name:     s.Get("name", s.Name),
			URL:      s.Get("url", ""),
			Content:  s.Get("content", ""),
			RefreshH: s.GetInt("refresh", 24),
		})
	}
	return c, nil
}

// Validate returns configuration errors (empty slice when valid). Only
// enabled tenants are validated against interface requirements, but
// structural checks apply to all.
func (c *Config) Validate() []error {
	var errs []error
	if len(c.Tenants) > MaxTenants {
		errs = append(errs, fmt.Errorf("too many tenants: %d > %d", len(c.Tenants), MaxTenants))
	}
	seen := map[string]bool{}
	subnetsOf := map[string][]*net.IPNet{}
	for _, t := range c.Tenants {
		pfx := fmt.Sprintf("tenant %q", t.Name)
		if !idRe.MatchString(t.ID) {
			errs = append(errs, fmt.Errorf("%s: bad id %q", pfx, t.ID))
		}
		if seen[t.ID] {
			errs = append(errs, fmt.Errorf("%s: duplicate id %q", pfx, t.ID))
		}
		seen[t.ID] = true
		if !t.Enabled {
			continue
		}
		switch t.EgressMode {
		case EgressInterface:
			if t.EgressInterface == "" {
				errs = append(errs, fmt.Errorf("%s: egress_interface is required", pfx))
			}
		case EgressVLESS:
			if t.VLESSURI != "" {
				if _, err := ParseVLESSURI(t.VLESSURI); err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", pfx, err))
				}
			} else {
				v := t.VLESS
				if v.UUID == "" || v.Server == "" || v.Port <= 0 {
					errs = append(errs, fmt.Errorf("%s: vless uuid/server/port are required", pfx))
				}
				switch v.Security {
				case "reality":
					if v.PublicKey == "" {
						errs = append(errs, fmt.Errorf("%s: reality requires public key (vless_pbk)", pfx))
					}
				case "tls":
					if v.SNI == "" {
						errs = append(errs, fmt.Errorf("%s: tls requires sni", pfx))
					}
				case "none":
				default:
					errs = append(errs, fmt.Errorf("%s: unknown vless_security %q", pfx, v.Security))
				}
			}
		default:
			errs = append(errs, fmt.Errorf("%s: bad egress_mode %q", pfx, t.EgressMode))
		}
		if t.Fallback != "main" && t.Fallback != "strict" {
			errs = append(errs, fmt.Errorf("%s: bad fallback %q", pfx, t.Fallback))
		}
		if t.RoutingMode != "select" && t.RoutingMode != "invert" {
			errs = append(errs, fmt.Errorf("%s: bad routing_mode %q", pfx, t.RoutingMode))
		}
		for _, n := range append(append([]string{}, t.IncludeSubnets...), t.ExcludeSubnets...) {
			if _, _, err := net.ParseCIDR(n); err != nil {
				errs = append(errs, fmt.Errorf("%s: bad subnet %q", pfx, n))
			}
		}
		for _, h := range append(append([]string{}, t.ExcludedHosts...), t.FullHosts...) {
			if net.ParseIP(h) == nil {
				errs = append(errs, fmt.Errorf("%s: bad host %q", pfx, h))
			}
		}
		if len(t.Networks) == 0 {
			errs = append(errs, fmt.Errorf("%s: at least one network is required", pfx))
		}
		for _, u := range t.DNSUpstream {
			if err := ValidateDNSUpstreamAddr(u); err != nil {
				errs = append(errs, fmt.Errorf("%s: dns_upstream %q: %v", pfx, u, err))
			}
		}
		if t.UpstreamPolicy != "sequential" && t.UpstreamPolicy != "random" {
			errs = append(errs, fmt.Errorf("%s: bad upstream_policy %q", pfx, t.UpstreamPolicy))
		}
		for _, r := range t.DNSRules {
			if r.Domain == "" || len(r.Targets) == 0 {
				errs = append(errs, fmt.Errorf("%s: dnsrule requires domain and target", pfx))
			}
			if r.Type != "record" && r.Type != "forward" {
				errs = append(errs, fmt.Errorf("%s: bad dnsrule type %q", pfx, r.Type))
			}
		}
		if len(t.FullHosts) > 0 && t.EgressMode == EgressVLESS && t.RoutingMode == "select" && len(t.IncludeSubnets)+len(t.IncludeDomains)+len(t.IncludeGeoIP)+len(t.IncludeLists) == 0 {
			// not an error: full hosts still work — resources list may be empty
			_ = t
		}
	}
	// Host-based source networks overlap check: two enabled tenants must not
	// claim the same uci network/device.
	own := map[string]string{}
	for _, t := range c.Tenants {
		if !t.Enabled {
			continue
		}
		for _, n := range t.Networks {
			n = strings.ToLower(n)
			if other, ok := own[n]; ok && other != t.ID {
				errs = append(errs, fmt.Errorf("network %q is claimed by tenants %q and %q", n, other, t.ID))
			}
			own[n] = t.ID
		}
	}
	_ = subnetsOf
	for _, l := range c.Lists {
		if l.Name == "" {
			errs = append(errs, fmt.Errorf("list: list sections require a name"))
		}
		// url empty means a manual list filled via the content option.
	}
	return errs
}

// EnabledTenants returns the enabled tenants in priority order.
func (c *Config) EnabledTenants() []*Tenant {
	var out []*Tenant
	for _, t := range c.Tenants {
		if t.Enabled {
			out = append(out, t)
		}
	}
	// stable sort by priority, then index
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if b.Priority < a.Priority || (b.Priority == a.Priority && b.Index < a.Index) {
				out[j-1], out[j] = b, a
			} else {
				break
			}
		}
	}
	return out
}

// List returns the list definition by name.
func (c *Config) List(name string) *ListDef {
	for _, l := range c.Lists {
		if l.Name == name {
			return l
		}
	}
	return nil
}
