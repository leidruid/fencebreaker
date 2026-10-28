package dnsgen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leidruid/fencebreaker/daemon/internal/model"
	"github.com/leidruid/fencebreaker/daemon/internal/nftgen"
	"github.com/leidruid/fencebreaker/daemon/internal/uci"
)

const testUCI = `
config fencebreaker 'global'
	option coredns_port '1053'
	option ttl_min '300'
	option ttl_max '86400'
	option resync_interval '60'

config tenant
	option id 'guest'
	option name 'Guest'
	option enabled '1'
	option egress_mode 'vless'
	option vless_server 's.example'
	option vless_port '443'
	option vless_uuid 'u'
	option vless_security 'reality'
	option vless_pbk 'k'
	option routing_mode 'select'
	list include_domains 'example.com'
	list exclude_domains 'bank.ru'
	list include_lists 'bl'
	list networks 'guest'
	list full_hosts '192.168.2.11'
	list dns_upstream '1.1.1.1'
	list dns_upstream 'tls://dns.adguard.com'
	option upstream_policy 'random'
	option dns_via_tunnel '1'
	option dns_keepalive '1'

config dnsrule
	option tenant 'guest'
	option type 'record'
	option domain 'api.corp'
	list target '10.0.0.5'

config dnsrule
	option tenant 'guest'
	option type 'forward'
	option domain 'corp.example'
	list target '10.10.0.1:5353'

config list
	option name 'bl'
	option url 'http://x'
`

func testCfg(t *testing.T) *model.Config {
	t.Helper()
	cfg, err := uci.Parse([]byte(testUCI))
	if err != nil {
		t.Fatal(err)
	}
	m, err := model.FromUCI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuild(t *testing.T) {
	st := Build(testCfg(t),
		map[string][]string{"guest": {"192.168.2.0/24"}},
		map[string]*nftgen.ParsedList{"bl": {Domains: []string{"dl.example"}}},
	)
	if st.ListenPort != 1053 || st.TTLMinSec != 300 || st.DefaultUpstreams[0] != "127.0.0.1:53" {
		t.Fatalf("globals wrong: %+v", st)
	}
	if len(st.Tenants) != 1 {
		t.Fatalf("tenants = %+v", st.Tenants)
	}
	ts := st.Tenants[0]
	if ts.Mark != 0x1000 || ts.Sets.IncDNS != "t1_inc_dns" || ts.LearningMode != "select" {
		t.Fatalf("tenant wrong: %+v", ts)
	}
	// upstream addresses pass through as-is (the plugin parses the 11
	// supported forms itself) and carry the tenant's selection policy
	if len(ts.Upstreams) != 2 || ts.Upstreams[0] != "1.1.1.1" || ts.Upstreams[1] != "tls://dns.adguard.com" {
		t.Fatalf("upstreams wrong: %+v", ts.Upstreams)
	}
	if ts.UpstreamPolicy != "random" || !ts.DNSViaTunnel {
		t.Fatalf("upstream policy/tunnel wrong: %+v", ts)
	}
	if !ts.DNSKeepAlive {
		t.Fatalf("dns_keepalive did not pass through: %+v", ts)
	}
	foundD, foundFwd, foundRec := false, false, false
	for _, d := range ts.IncludeDomains {
		if d == "dl.example" {
			foundD = true
		}
	}
	for _, f := range ts.Forwards {
		if f.Domain == "corp.example." && f.Servers[0] == "10.10.0.1:5353" {
			foundFwd = true
		}
	}
	for _, r := range ts.Records {
		if r.Domain == "api.corp." && r.IPs[0] == "10.0.0.5" {
			foundRec = true
		}
	}
	if !foundD || !foundFwd || !foundRec {
		t.Fatalf("domains/forwards/records wrong: %+v", ts)
	}
	if len(ts.HostsFull) != 1 || ts.HostsFull[0] != "192.168.2.11" {
		t.Fatalf("hosts_full = %v", ts.HostsFull)
	}
}

func TestUpstreamPolicyDefault(t *testing.T) {
	/* no upstream_policy option: the state must carry "sequential" */
	cfg, err := uci.Parse([]byte(strings.Replace(testUCI, "option upstream_policy 'random'", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	m, err := model.FromUCI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st := Build(m, map[string][]string{"guest": {"192.168.2.0/24"}}, nil)
	if len(st.Tenants) != 1 || st.Tenants[0].UpstreamPolicy != "sequential" {
		t.Fatalf("upstream_policy default wrong: %+v", st.Tenants)
	}
}

func TestWriteAndMappingChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dns_state.json")
	st := Build(testCfg(t), map[string][]string{"guest": {"192.168.2.0/24"}}, nil)
	st.Generation = 7
	if err := Write(path, st); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back["generation"].(float64) != 7 {
		t.Fatalf("generation not written: %s", raw)
	}

	same := map[string][2]string{"guest": {"t1_inc_dns", "t1_exc_dns"}}
	if MappingChanged(st, same) {
		t.Fatal("same mapping reported changed")
	}
	if !MappingChanged(st, map[string][2]string{"guest": {"t2_inc_dns", "t2_exc_dns"}}) {
		t.Fatal("changed set names not detected")
	}
	if !MappingChanged(st, map[string][2]string{}) {
		t.Fatal("missing tenant not detected")
	}
}
