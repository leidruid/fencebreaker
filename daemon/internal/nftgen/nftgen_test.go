package nftgen

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/leidruid/fencebreaker/daemon/internal/model"
	"github.com/leidruid/fencebreaker/daemon/internal/uci"
)

func testInputs(t *testing.T) *Inputs {
	t.Helper()
	src := `
config fencebreaker 'global'
	option coredns_port '1053'
	option ttl_max '86400'

config tenant
	option id 'guest'
	option name 'Guest'
	option enabled '1'
	option egress_mode 'interface'
	option egress_interface 'wan2'
	option routing_mode 'invert'
	list exclude_subnets '10.0.0.0/8'
	list networks 'guest'
	list excluded_hosts '192.168.2.10'
	list full_hosts '192.168.2.11'
	option dns_hijack '1'
`
	cfg, err := uci.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m, err := model.FromUCI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &Inputs{
		Config:     m,
		Lists:      map[string]*ParsedList{},
		GeoIP:      map[string][]string{"ru": {"45.135.240.0/22", "5.45.112.0/20"}},
		DNSRestore: map[string][]Elem{},
		SrcNets:    map[string][]string{"guest": {"192.168.2.0/24"}},
	}
}

func TestRenderStructure(t *testing.T) {
	in := testInputs(t)
	in.DNSRestore["t1_inc_dns"] = []Elem{{IP: "1.2.3.4", TimeoutSec: 300}}
	out := Render(in)
	checks := []string{
		"table inet fencebreaker {",
		"set t1_src_nets {",
		"set t1_inc_dns {",
		"flags timeout;",
		"timeout 86400s;",
		"1.2.3.4 timeout 300s",
		"type nat hook prerouting priority -150; policy accept;",
		"ip saddr @t1_hosts_exc udp dport 53 return",
		"ip saddr @t1_src_nets udp dport 53 redirect to :1053",
		"ip saddr @t1_src_nets tcp dport 53 redirect to :1053",
		"type filter hook prerouting priority -150; policy accept;",
		"ip saddr @t1_src_nets jump t1_fwd",
		"ip saddr @t1_hosts_full jump t1_fwd",
		"ip saddr @t1_hosts_exc return",
		"ip saddr @t1_hosts_full meta mark set 0x1000 return",
		"ip daddr @t1_exc_static return",
		"ip daddr @t1_inc_dns meta mark set 0x1000 return",
		"meta mark set 0x1000 return", // invert: mark everything else
	}
	for _, c := range checks {
		if !strings.Contains(out, c) {
			t.Errorf("ruleset missing %q", c)
		}
	}
	// exc rules must precede inc rules in the tenant chain
	if i, j := strings.Index(out, "ip daddr @t1_exc_static return"), strings.Index(out, "ip daddr @t1_inc_static meta mark set"); i > j {
		t.Error("exclude rules must come before include rules")
	}
	if strings.Contains(out, "elements = {  }") {
		t.Error("empty elements block rendered")
	}
}

func TestRenderSelectModeHasNoCatchAll(t *testing.T) {
	in := testInputs(t)
	src := `
config fencebreaker 'global'
config tenant
	option id 'guest'
	option enabled '1'
	option egress_mode 'interface'
	option egress_interface 'wan2'
	option routing_mode 'select'
	list networks 'guest'
`
	cfg, _ := uci.Parse([]byte(src))
	m, _ := model.FromUCI(cfg)
	in.Config = m
	out := Render(in)
	if strings.Contains(out, "# invert") {
		t.Error("select mode must not render catch-all mark")
	}
	if strings.Count(out, "meta mark set") != 4 { // hosts_full + 3 include sets
		t.Errorf("unexpected mark rules:\n%s", out)
	}
}

func TestRenderNoTenants(t *testing.T) {
	cfg, _ := uci.Parse([]byte("config fencebreaker 'global'\n"))
	m, _ := model.FromUCI(cfg)
	if out := Render(&Inputs{Config: m}); out != "" {
		t.Errorf("expected empty ruleset, got:\n%s", out)
	}
}

func TestRenderGeoIPAndLists(t *testing.T) {
	in := testInputs(t)
	in.Lists["blocked"] = &ParsedList{Nets: []string{"9.9.9.0/24"}, Domains: []string{"x.com"}}
	in.Config.Tenants[0].IncludeGeoIP = []string{"ru", "zz"}
	in.Config.Tenants[0].IncludeLists = []string{"blocked"}
	in.Config.Tenants[0].RoutingMode = "select"
	out := Render(in)
	if !strings.Contains(out, "9.9.9.0/24") {
		t.Error("list nets missing from inc_static")
	}
	if !strings.Contains(out, "45.135.240.0/22") {
		t.Error("geoip cidrs missing")
	}
	if !strings.Contains(out, `geoip zone "zz" is not loaded`) {
		t.Error("missing geoip warning comment")
	}
}

// TestNftCheck validates the rendered ruleset with the nft binary when the
// environment allows it (nft -c needs CAP_NET_ADMIN and kernel nftables).
func TestNftCheck(t *testing.T) {
	nftPath, err := exec.LookPath("nft")
	if err != nil {
		t.Skip("nft not installed")
	}
	if out, err := exec.Command(nftPath, "list", "tables").CombinedOutput(); err != nil {
		t.Skipf("nftables unavailable: %v: %s", err, out)
	}
	in := testInputs(t)
	in.DNSRestore["t1_inc_dns"] = []Elem{{IP: "1.2.3.4", TimeoutSec: 300}}
	out := Render(in)
	// wrap in a fresh table creation to keep -c side-effect free
	cmd := exec.Command(nftPath, "-c", "-f", "-")
	cmd.Stdin = strings.NewReader(out)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("nft -c rejected ruleset: %v\n%s\n---ruleset---\n%s", err, o, out)
	}
}
