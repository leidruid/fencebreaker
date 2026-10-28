package model

import (
	"strings"
	"testing"

	"github.com/leidruid/fencebreaker/daemon/internal/uci"
)

const sampleConf = `
config fencebreaker 'global'
	option enabled '1'
	option coredns_port '1053'

config tenant
	option id 'guest'
	option name 'Guest'
	option enabled '1'
	option egress_mode 'interface'
	option egress_interface 'wan2'
	option routing_mode 'select'
	list include_subnets '1.2.3.0/24'
	list include_geoip 'ru'
	list networks 'guest'
	list excluded_hosts '192.168.2.10'
	list full_hosts '192.168.2.11'
	list dns_upstream '1.1.1.1'

config tenant
	option id 'corp'
	option name 'Corp'
	option enabled '0'

config dnsrule
	option tenant 'guest'
	option type 'record'
	option domain 'api.corp'
	list target '10.0.0.5'

config list 'blocked'
	option name 'blocked'
	option url 'https://example.com/l.txt'
`

func TestFromUCI(t *testing.T) {
	cfg, err := uci.Parse([]byte(sampleConf))
	if err != nil {
		t.Fatal(err)
	}
	m, err := FromUCI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if m.Global.CoreDNSPort != 1053 || !m.Global.Enabled {
		t.Fatalf("global wrong: %+v", m.Global)
	}
	if len(m.Tenants) != 2 {
		t.Fatalf("want 2 tenants, got %d", len(m.Tenants))
	}
	g := m.Tenants[0]
	if g.ID != "guest" || g.Index != 1 || g.TableID() != 101 || g.Mark() != 0x1000 {
		t.Fatalf("guest tenant wrong: %+v", g)
	}
	if g.RulePref() != 11010 {
		t.Fatalf("rule pref = %d", g.RulePref())
	}
	if g.Sets().IncDNS != "t1_inc_dns" || g.TunName() != "fb-tun1" {
		t.Fatalf("set names wrong: %+v", g.Sets())
	}
	if len(g.DNSRules) != 1 || g.DNSRules[0].Domain != "api.corp" {
		t.Fatalf("dnsrules wrong: %+v", g.DNSRules)
	}
	if len(m.Lists) != 1 || m.Lists[0].URL == "" {
		t.Fatalf("lists wrong: %+v", m.Lists)
	}
	et := m.EnabledTenants()
	if len(et) != 1 || et[0].ID != "guest" {
		t.Fatalf("enabled tenants wrong: %+v", et)
	}
}

func TestValidate(t *testing.T) {
	src := strings.Replace(sampleConf, "list networks 'guest'", "", 1)
	cfg, _ := uci.Parse([]byte(src))
	m, _ := FromUCI(cfg)
	errs := m.Validate()
	if len(errs) == 0 {
		t.Fatal("expected error for missing networks")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "at least one network") {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors: %v", errs)
	}

	// full valid config must produce no errors
	cfg2, _ := uci.Parse([]byte(sampleConf))
	m2, _ := FromUCI(cfg2)
	if errs := m2.Validate(); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// network overlap between two enabled tenants
	src2 := strings.Replace(sampleConf, "option enabled '0'", "option enabled '1'\n\tlist networks 'guest'", 1)
	cfg3, _ := uci.Parse([]byte(src2))
	m3, _ := FromUCI(cfg3)
	errs = m3.Validate()
	ok := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "is claimed by tenants") {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("expected overlap error, got: %v", errs)
	}
}

func TestVLESSValidation(t *testing.T) {
	cfg, _ := uci.Parse([]byte(`
config tenant
	option id 'v'
	option enabled '1'
	option egress_mode 'vless'
	list networks 'lan'
	option vless_uuid 'uuid'
	option vless_server 's'
	option vless_port '443'
`))
	m, _ := FromUCI(cfg)
	errs := m.Validate()
	if len(errs) == 0 {
		t.Fatal("expected reality public key error")
	}
}
