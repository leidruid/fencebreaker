package fbtenant

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---- state loading ---------------------------------------------------------

func TestLoadStateFileFromContractExample(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dns_state.json")
	json := `{
	  "version": 1,
	  "generation": 3,
	  "listen_port": 1053,
	  "resync_interval_sec": 60,
	  "ttl_min_sec": 300,
	  "ttl_max_sec": 86400,
	  "refresh_on_remain_pct": 0,
	  "max_set_size": 65536,
	  "default_upstreams": ["127.0.0.1:53"],
	  "nft_table": "fencebreaker",
	  "tenants": [
	    {
	      "id": "tenant1",
	      "mark": 4096,
	      "src_nets": ["192.168.100.0/24"],
	      "hosts_full": ["192.168.1.60"],
	      "sets": {"inc_dns": "t1_inc_dns", "exc_dns": "t1_exc_dns"},
	      "learning_mode": "select",
	      "include_domains": ["example.com"],
	      "exclude_domains": ["bank.ru"],
	      "upstreams": ["1.1.1.1:53"],
	      "dns_via_tunnel": true,
	      "records": [ {"domain": "api.corp", "ips": ["10.0.0.5"]} ],
	      "forwards": [ {"domain": "corp.example", "servers": ["10.10.0.1:53"]} ]
	    }
	  ]
	}`
	if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := loadStateFile(path)
	if err != nil {
		t.Fatalf("loadStateFile: %v", err)
	}
	if st.Generation != 3 || st.NFTTable != "fencebreaker" || len(st.Tenants) != 1 {
		t.Fatalf("unexpected state: %+v", st)
	}
	tn := st.Tenants[0]
	if tn.Mark != 4096 || !tn.DNSViaTunnel || tn.Sets["inc_dns"] != "t1_inc_dns" {
		t.Fatalf("unexpected tenant: %+v", tn)
	}
	cfg, err := prepare(st)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if cfg.tenantsByIPClient == nil {
		t.Fatal("nil router")
	}
	if got := cfg.tenantsByIPClient.lookup(mustIP("192.168.1.60")); got == nil || got.raw.ID != "tenant1" {
		t.Fatalf("hosts_full lookup failed: %v", got)
	}
	if got := cfg.tenantsByIPClient.lookup(mustIP("192.168.100.9")); got == nil || got.raw.ID != "tenant1" {
		t.Fatalf("src_nets lookup failed: %v", got)
	}
	if got := cfg.tenantsByIPClient.lookup(mustIP("10.9.9.9")); got != nil {
		t.Fatalf("expected unknown client, got %v", got.raw.ID)
	}
}

// ---- tenant selection: hosts_full beats LPM, longest prefix wins -----------

func TestTenantSelectionTable(t *testing.T) {
	st := &State{
		Version: 1, NFTTable: "fencebreaker",
		Tenants: []Tenant{
			{
				ID: "wide", SrcNets: []string{"10.0.0.0/8"},
				Sets: map[string]string{"inc_dns": "t1_inc_dns"},
			},
			{
				ID: "narrow", SrcNets: []string{"10.1.0.0/16"},
				Sets: map[string]string{"inc_dns": "t2_inc_dns"},
			},
			{
				ID: "fullhost", HostsFull: []string{"10.1.2.3"},
				Sets: map[string]string{"inc_dns": "t3_inc_dns"},
			},
		},
	}
	cfg, err := prepare(st)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		ip   string
		want string // tenant id, "" for none
	}{
		{"10.1.2.3", "fullhost"}, // exact hosts_full beats both prefixes
		{"10.1.9.9", "narrow"},   // LPM: /16 beats /8
		{"10.2.0.1", "wide"},
		{"192.168.0.1", ""},
	}
	for _, tc := range tests {
		got := cfg.tenantsByIPClient.lookup(mustIP(tc.ip))
		var gotID string
		if got != nil {
			gotID = got.raw.ID
		}
		if gotID != tc.want {
			t.Errorf("lookup(%s) = %q, want %q", tc.ip, gotID, tc.want)
		}
	}
}

// ---- forwards trie: narrowest suffix wins ----------------------------------

func TestForwardTrieNarrowestSuffix(t *testing.T) {
	tr := newFwdTrie()
	tr.insert("corp.example", []string{"wide:53"})
	tr.insert("api.corp.example", []string{"narrow:53"})
	tr.insert("other.com", []string{"other:53"})

	tests := []struct {
		qname string
		want  string
	}{
		{"x.api.corp.example", "narrow:53"},
		{"api.corp.example", "narrow:53"},
		{"www.corp.example", "wide:53"},
		{"corp.example", "wide:53"},
		{"other.com", "other:53"},
		{"sub.other.com", "other:53"},
		{"unrelated.net", ""},
		{"example.com", ""}, // suffix != full label match
	}
	for _, tc := range tests {
		got := tr.lookup(tc.qname)
		var gotStr string
		if len(got) > 0 {
			gotStr = got[0]
		}
		if gotStr != tc.want {
			t.Errorf("lookup(%q) = %q, want %q", tc.qname, gotStr, tc.want)
		}
	}
}

// ---- learning target: select / invert / excluded ---------------------------

func TestLearnTargetTable(t *testing.T) {
	mk := func(mode string) *preparedTenant {
		return &preparedTenant{raw: Tenant{
			LearningMode:   mode,
			IncludeDomains: []string{"example.com", "corp.local"},
			ExcludeDomains: []string{"bank.ru"},
		}}
	}
	tests := []struct {
		mode  string
		qname string
		want  string
	}{
		{"select", "www.example.com", SetIncDNS},
		{"select", "example.com", SetIncDNS},
		// excluded suffixes always learn into exc_dns: in invert mode the
		// catch-all mark must not capture them, in select mode an exclude
		// must override an include subnet
		{"select", "bank.ru", SetExcDNS},
		{"select", "sub.bank.ru", SetExcDNS},
		{"select", "other.net", ""},
		{"select", "notexample.com", ""}, // suffix must be label-aligned
		{"invert", "www.example.com", SetIncDNS},
		{"invert", "anything.net", SetIncDNS},
		{"invert", "bank.ru", SetExcDNS},
		{"invert", "sub.bank.ru", SetExcDNS},
		{"unknown", "www.example.com", ""},
	}
	for _, tc := range tests {
		if got := mk(tc.mode).learnTarget(tc.qname); got != tc.want {
			t.Errorf("learnTarget(%s, %q) = %q, want %q", tc.mode, tc.qname, got, tc.want)
		}
	}
}

// ---- TTL clamping ----------------------------------------------------------

func TestClampTTLTable(t *testing.T) {
	min, max := 300*time.Second, 86400*time.Second
	tests := []struct {
		qttl time.Duration
		want time.Duration
	}{
		{0, min},
		{-5 * time.Second, min},
		{30 * time.Second, min},
		{300 * time.Second, 300 * time.Second},
		{1200 * time.Second, 1200 * time.Second},
		{200000 * time.Second, max},
	}
	for _, tc := range tests {
		if got := clampTTL(tc.qttl, min, max); got != tc.want {
			t.Errorf("clampTTL(%v) = %v, want %v", tc.qttl, got, tc.want)
		}
	}
}

// ---- matchSuffix -----------------------------------------------------------

func TestMatchSuffix(t *testing.T) {
	tests := []struct {
		domain, suffix string
		want           bool
	}{
		{"www.example.com", "example.com", true},
		{"example.com", "example.com", true},
		{"notexample.com", "example.com", false},
		{"example.com.evil.ru", "example.com", false},
		{"anything", "", true},
	}
	for _, tc := range tests {
		if got := matchSuffix(tc.domain, tc.suffix); got != tc.want {
			t.Errorf("matchSuffix(%q, %q) = %v, want %v", tc.domain, tc.suffix, got, tc.want)
		}
	}
}

func mustIP(s string) net.IP {
	ip := net.ParseIP(s)
	if ip == nil {
		panic("bad test ip " + s)
	}
	return ip
}
