package fbtenant

import (
	"testing"
)

func TestParseUpstreamForms(t *testing.T) {
	cases := []struct {
		raw     string
		scheme  string
		host    string
		port    int
		path    string
		hostIsN bool
	}{
		{"94.140.14.140", SchemeUDP, "94.140.14.140", 53, "", false},
		{"2a10:50c0::1:ff", SchemeUDP, "2a10:50c0::1:ff", 53, "", false},
		{"94.140.14.140:53", SchemeUDP, "94.140.14.140", 53, "", false},
		{"[2a10:50c0::1:ff]:53", SchemeUDP, "2a10:50c0::1:ff", 53, "", false},
		{"udp://unfiltered.adguard-dns.com", SchemeUDP, "unfiltered.adguard-dns.com", 53, "", true},
		{"udp://[2001:db8::1]:5353", SchemeUDP, "2001:db8::1", 5353, "", false},
		{"tcp://94.140.14.140", SchemeTCP, "94.140.14.140", 53, "", false},
		{"tcp://[2a10:50c0::1:ff]:53", SchemeTCP, "2a10:50c0::1:ff", 53, "", false},
		{"tcp://unfiltered.adguard-dns.com", SchemeTCP, "unfiltered.adguard-dns.com", 53, "", true},
		{"tls://unfiltered.adguard-dns.com", SchemeTLS, "unfiltered.adguard-dns.com", 853, "", true},
		{"tls://dns.example:8530", SchemeTLS, "dns.example", 8530, "", true},
		{"https://unfiltered.adguard-dns.com/dns-query", SchemeHTTPS, "unfiltered.adguard-dns.com", 443, "/dns-query", true},
		{"https://dns.example:8443/custom", SchemeHTTPS, "dns.example", 8443, "/custom", true},
		{"https://dns.example", SchemeHTTPS, "dns.example", 443, "/dns-query", true},
		{"h3://unfiltered.adguard-dns.com/dns-query", SchemeH3, "unfiltered.adguard-dns.com", 443, "/dns-query", true},
		{"quic://unfiltered.adguard-dns.com", SchemeQUIC, "unfiltered.adguard-dns.com", 853, "", true},
		{"quic://dns.example:8853", SchemeQUIC, "dns.example", 8853, "", true},
	}
	for _, c := range cases {
		sp, err := ParseUpstream(c.raw)
		if err != nil {
			t.Errorf("parse %q: %v", c.raw, err)
			continue
		}
		if sp.Scheme != c.scheme || sp.Host != c.host || sp.Port != c.port || sp.Path != c.path || sp.HostIsName != c.hostIsN {
			t.Errorf("parse %q = %+v, want scheme=%s host=%s port=%d path=%q name=%v",
				c.raw, sp, c.scheme, c.host, c.port, c.path, c.hostIsN)
		}
	}

	sp, err := ParseUpstream("sdns://AQcAAAAAAAAADzIxMi40Ny4yMjAuMTcy")
	if err != nil || sp.Scheme != SchemeSDNS || sp.Stamp == "" {
		t.Fatalf("sdns parse: %+v err=%v", sp, err)
	}
	if sp.Addr() != "sdns://AQcAAAAAAAAADzIxMi40Ny4yMjAuMTcy" {
		t.Fatalf("sdns addr: %s", sp.Addr())
	}
}

func TestParseUpstreamErrors(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"http://example.com",
		"udp://",
		"tcp://host/path",
		"udp://host:notaport",
		"94.140.14.140:99999",
		"94.140.14.140:",
		"[bad",
		"[300::1]:53:54",
		"sdns://",
		"host:53:54",
		"-bad.host",
	} {
		if _, err := ParseUpstream(raw); err == nil {
			t.Errorf("parse %q: expected error", raw)
		}
	}
}

func TestUpstreamURL(t *testing.T) {
	sp, err := ParseUpstream("h3://dns.example:8443/x?k=v")
	if err != nil {
		t.Fatal(err)
	}
	if got := sp.URL(); got != "h3://dns.example:8443/x?k=v" {
		t.Fatalf("URL = %s", got)
	}
}

func TestNormalizePolicy(t *testing.T) {
	for in, want := range map[string]string{
		"":           PolicySequential,
		"random":     PolicyRandom,
		"RANDOM":     PolicyRandom,
		"sequential": PolicySequential,
		"bogus":      PolicySequential,
	} {
		if got := NormalizePolicy(in); got != want {
			t.Errorf("NormalizePolicy(%q) = %s, want %s", in, got, want)
		}
	}
}
