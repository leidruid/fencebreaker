package uci

import (
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, src string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

func TestParseBasics(t *testing.T) {
	cfg := mustParse(t, `
# comment
config fencebreaker 'global'
	option enabled '1'
	option coredns_port '1053'
	list dns_upstream '1.1.1.1'
	list dns_upstream "8.8.8.8"

config tenant
	option id 'tenant1'
	option name 'My Tenant'
	list networks guest
`)
	g := cfg.Named("fencebreaker", "global")
	if g == nil {
		t.Fatal("global section not found")
	}
	if g.Get("enabled", "") != "1" || g.GetInt("coredns_port", 0) != 1053 {
		t.Fatalf("global options wrong: %+v", g.Options)
	}
	if !g.GetBool("enabled", false) {
		t.Fatal("enabled should be true")
	}
	if !reflect.DeepEqual(g.List("dns_upstream"), []string{"1.1.1.1", "8.8.8.8"}) {
		t.Fatalf("list wrong: %v", g.List("dns_upstream"))
	}
	tenants := cfg.SectionsOfType("tenant")
	if len(tenants) != 1 {
		t.Fatalf("want 1 tenant, got %d", len(tenants))
	}
	tt := tenants[0]
	if tt.Name != "anon1" {
		t.Fatalf("anonymous section name = %q", tt.Name)
	}
	if tt.Get("name", "") != "My Tenant" {
		t.Fatalf("quoted value with spaces wrong: %q", tt.Get("name", ""))
	}
	if !reflect.DeepEqual(tt.List("networks"), []string{"guest"}) {
		t.Fatalf("unquoted list wrong: %v", tt.List("networks"))
	}
}

func TestParseErrors(t *testing.T) {
	_, err := Parse([]byte("option foo 'bar'\nconfig 'x'\n"))
	if err == nil {
		t.Fatal("expected errors for option outside section and config without type")
	}
	if !strings.Contains(err.Error(), "option outside section") {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = Parse([]byte("option x 'abc"))
	if err == nil || !strings.Contains(err.Error(), "unterminated quote") {
		t.Fatalf("expected unterminated quote, got %v", err)
	}
}

func TestGetDefaults(t *testing.T) {
	cfg := mustParse(t, "config t 's'\n\toption n 'no'\n")
	s := cfg.Named("t", "s")
	if s.Get("missing", "d") != "d" {
		t.Fatal("Get default broken")
	}
	if s.GetBool("n", true) {
		t.Fatal("GetBool should parse 'no' as false")
	}
	if s.GetInt("missing", 42) != 42 {
		t.Fatal("GetInt default broken")
	}
	if s.List("missing") == nil {
		t.Fatal("List must never return nil")
	}
}
