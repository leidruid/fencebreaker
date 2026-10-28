package fbtenant

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// State mirrors dns_state.json written atomically by the fencebreaker daemon
// (CONTRACTS §2).
type State struct {
	Version            int      `json:"version"`
	Generation         int64    `json:"generation"`
	ListenPort         int      `json:"listen_port"`
	ResyncIntervalSec  int      `json:"resync_interval_sec"`
	TTLMinSec          int      `json:"ttl_min_sec"`
	TTLMaxSec          int      `json:"ttl_max_sec"`
	RefreshOnRemainPct int      `json:"refresh_on_remain_pct"`
	MaxSetSize         int      `json:"max_set_size"`
	DefaultUpstreams   []string `json:"default_upstreams"`
	NFTTable           string   `json:"nft_table"`
	Tenants            []Tenant `json:"tenants"`
}

// Tenant is one dns_state.json tenant entry.
type Tenant struct {
	ID             string            `json:"id"`
	Mark           uint32            `json:"mark"`
	SrcNets        []string          `json:"src_nets"`
	HostsFull      []string          `json:"hosts_full"`
	Sets           map[string]string `json:"sets"`
	LearningMode   string            `json:"learning_mode"`
	IncludeDomains []string          `json:"include_domains"`
	ExcludeDomains []string          `json:"exclude_domains"`
	Upstreams      []string          `json:"upstreams"`
	UpstreamPolicy string            `json:"upstream_policy"`
	DNSViaTunnel   bool              `json:"dns_via_tunnel"`
	DNSKeepAlive   bool              `json:"dns_keepalive"`
	Records        []Record          `json:"records"`
	Forwards       []Forward         `json:"forwards"`
}

// Record is an exact A record override.
type Record struct {
	Domain string   `json:"domain"`
	IPs    []string `json:"ips"`
}

// Forward is a per-suffix zone forward.
type Forward struct {
	Domain  string   `json:"domain"`
	Servers []string `json:"servers"`
}

// Set kinds managed by the plugin (subset of tenant.sets).
const (
	SetIncDNS = "inc_dns"
	SetExcDNS = "exc_dns"
)

// preparedTenant is the runtime, pre-parsed view of a Tenant.
type preparedTenant struct {
	raw Tenant

	nets      []*net.IPNet // parsed src_nets, longest first is irrelevant (LPM computed per query)
	hostsFull map[string]bool
	records   map[string][]net.IP
	forwards  *fwdTrie
	upstreams []string
	// upstreamPolicy is the normalized upstream selection order.
	upstreamPolicy string
	// keepalive enables the proactive domain keeper (CONTRACTS §3).
	keepalive bool

	stats *tenantStats
	cache *tenantCache
}

// config is the prepared plugin configuration built from State.
type config struct {
	state             State
	generation        int64
	table             string
	defaultUpstreams  []string
	tenantsByIPClient *tenantRouter
	tenants           []*preparedTenant
}

// tenantRouter resolves a client IP to a tenant: exact hosts_full hit wins,
// otherwise longest-prefix-match across all tenants' src_nets.
type tenantRouter struct {
	exact map[string]*preparedTenant
	nets  []netRoute
}

type netRoute struct {
	net    *net.IPNet
	tenant *preparedTenant
}

func (r *tenantRouter) lookup(ip net.IP) *preparedTenant {
	if r == nil {
		return nil
	}
	if ip == nil {
		return nil
	}
	if t, ok := r.exact[ip.String()]; ok {
		return t
	}
	var best *preparedTenant
	bestBits := -1
	for _, nr := range r.nets {
		if !nr.net.Contains(ip) {
			continue
		}
		bits, _ := nr.net.Mask.Size()
		if bits > bestBits {
			best, bestBits = nr.tenant, bits
		}
	}
	return best
}

// fwdTrie matches the narrowest (longest) configured suffix of a domain.
type fwdTrie struct {
	root *fwdNode
}

type fwdNode struct {
	children map[string]*fwdNode
	servers  []string
}

func newFwdTrie() *fwdTrie { return &fwdTrie{root: &fwdNode{children: map[string]*fwdNode{}}} }

func (t *fwdTrie) insert(domain string, servers []string) {
	node := t.root
	labels := splitLabels(domain)
	for i := len(labels) - 1; i >= 0; i-- {
		child, ok := node.children[labels[i]]
		if !ok {
			child = &fwdNode{children: map[string]*fwdNode{}}
			node.children[labels[i]] = child
		}
		node = child
	}
	node.servers = servers
}

// lookup returns servers of the narrowest matching suffix, nil if none.
func (t *fwdTrie) lookup(qname string) []string {
	node := t.root
	labels := splitLabels(qname)
	var best []string
	for i := len(labels) - 1; i >= 0; i-- {
		child, ok := node.children[labels[i]]
		if !ok {
			break
		}
		node = child
		if len(node.servers) > 0 {
			best = node.servers
		}
	}
	return best
}

func splitLabels(domain string) []string {
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if domain == "" {
		return nil
	}
	return strings.Split(domain, ".")
}

// matchSuffix reports whether domain equals suffix or is a subdomain of it.
func matchSuffix(domain, suffix string) bool {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	suffix = strings.ToLower(strings.TrimSuffix(suffix, "."))
	if suffix == "" {
		return true
	}
	return domain == suffix || strings.HasSuffix(domain, "."+suffix)
}

// learnTarget decides which nft set an answer's A records belong to
// (CONTRACTS §2/§3 semantics):
//   - excluded suffixes → exc_dns: their traffic must bypass the tenant
//     channel — in invert mode the catch-all mark would otherwise capture
//     them, and in select mode an exclude must override an include subnet;
//   - select mode: include suffixes → inc_dns, others not learned;
//   - invert mode: everything else → inc_dns.
func (t *preparedTenant) learnTarget(qname string) string {
	for _, d := range t.raw.ExcludeDomains {
		if matchSuffix(qname, d) {
			return SetExcDNS
		}
	}
	switch t.raw.LearningMode {
	case "select":
		for _, d := range t.raw.IncludeDomains {
			if matchSuffix(qname, d) {
				return SetIncDNS
			}
		}
		return ""
	case "invert":
		return SetIncDNS
	default:
		return ""
	}
}

// setName returns the nftables set name for a set kind, or "".
func (t *preparedTenant) setName(kind string) string {
	if t.raw.Sets == nil {
		return ""
	}
	return t.raw.Sets[kind]
}

// loadStateFile reads and parses dns_state.json.
func loadStateFile(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Version != 1 {
		return nil, fmt.Errorf("unsupported state version %d", st.Version)
	}
	return &st, nil
}

// prepare validates State and builds the runtime config.
func prepare(st *State) (*config, error) {
	cfg := &config{
		state:      *st,
		generation: st.Generation,
		table:      st.NFTTable,
	}
	for _, u := range st.DefaultUpstreams {
		if u = strings.TrimSpace(u); u != "" {
			cfg.defaultUpstreams = append(cfg.defaultUpstreams, u)
		}
	}
	router := &tenantRouter{exact: map[string]*preparedTenant{}}
	for i := range st.Tenants {
		t := &st.Tenants[i]
		if t.ID == "" {
			return nil, fmt.Errorf("tenant #%d: empty id", i)
		}
		pt, err := prepareTenant(*t)
		if err != nil {
			return nil, fmt.Errorf("tenant %q: %w", t.ID, err)
		}
		cfg.tenants = append(cfg.tenants, pt)
		for _, h := range pt.raw.HostsFull {
			router.exact[h] = pt
		}
		for _, n := range pt.nets {
			router.nets = append(router.nets, netRoute{net: n, tenant: pt})
		}
	}
	cfg.tenantsByIPClient = router
	return cfg, nil
}

func prepareTenant(t Tenant) (*preparedTenant, error) {
	pt := &preparedTenant{
		raw:       t,
		hostsFull: map[string]bool{},
		records:   map[string][]net.IP{},
		forwards:  newFwdTrie(),
		stats:     &tenantStats{},
		cache:     newTenantCache(maxCacheEntries),
	}
	for _, h := range t.HostsFull {
		ip := net.ParseIP(strings.TrimSpace(h)).To4()
		if ip == nil {
			return nil, fmt.Errorf("hosts_full: bad ip %q", h)
		}
		pt.hostsFull[ip.String()] = true
	}
	for _, n := range t.SrcNets {
		n = strings.TrimSpace(n)
		if !strings.Contains(n, "/") {
			n += "/32"
		}
		_, ipnet, err := net.ParseCIDR(n)
		if err != nil {
			return nil, fmt.Errorf("src_nets: bad cidr %q", n)
		}
		pt.nets = append(pt.nets, ipnet)
	}
	for _, r := range t.Records {
		domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(r.Domain), "."))
		if domain == "" {
			return nil, fmt.Errorf("records: empty domain")
		}
		var ips []net.IP
		for _, s := range r.IPs {
			ip := net.ParseIP(strings.TrimSpace(s)).To4()
			if ip == nil {
				return nil, fmt.Errorf("records %q: bad ip %q", r.Domain, s)
			}
			ips = append(ips, ip)
		}
		pt.records[domain] = ips
	}
	for _, f := range t.Forwards {
		domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(f.Domain), "."))
		if domain == "" {
			return nil, fmt.Errorf("forwards: empty domain")
		}
		var servers []string
		for _, s := range f.Servers {
			if s = strings.TrimSpace(s); s != "" {
				servers = append(servers, s)
			}
		}
		if len(servers) > 0 {
			pt.forwards.insert(domain, servers)
		}
	}
	for _, u := range t.Upstreams {
		if u = strings.TrimSpace(u); u != "" {
			pt.upstreams = append(pt.upstreams, u)
		}
	}
	pt.upstreamPolicy = NormalizePolicy(t.UpstreamPolicy)
	pt.keepalive = t.DNSKeepAlive
	return pt, nil
}

// clampTTL clamps the query TTL into [min, max]; qttl <= 0 yields min.
func clampTTL(qttl time.Duration, min, max time.Duration) time.Duration {
	if min <= 0 {
		min = time.Second
	}
	if max < min {
		max = min
	}
	if qttl <= 0 {
		return min
	}
	if qttl < min {
		return min
	}
	if qttl > max {
		return max
	}
	return qttl
}
