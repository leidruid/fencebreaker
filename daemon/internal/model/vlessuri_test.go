package model

import (
	"strings"
	"testing"
)

func TestParseVLESSURIReality(t *testing.T) {
	v, err := ParseVLESSURI("VLESS://11111111-2222-3333-4444-555555555555@example.org:443?type=tcp&security=reality&sni=example.org&fp=firefox&pbk=PUBKEY&sid=abcd&flow=xtls-rprx-vision#my-server")
	if err != nil {
		t.Fatal(err)
	}
	if v.UUID != "11111111-2222-3333-4444-555555555555" || v.Server != "example.org" || v.Port != 443 {
		t.Fatalf("basic fields wrong: %+v", v)
	}
	if v.Security != "reality" || v.SNI != "example.org" || v.Fingerprint != "firefox" {
		t.Fatalf("tls fields wrong: %+v", v)
	}
	if v.PublicKey != "PUBKEY" || v.ShortID != "abcd" || v.Flow != "xtls-rprx-vision" {
		t.Fatalf("reality fields wrong: %+v", v)
	}
}

func TestParseVLESSURIDefaults(t *testing.T) {
	v, err := ParseVLESSURI("vless://uuid@10.0.0.1:8443?security=reality&pbk=K")
	if err != nil {
		t.Fatal(err)
	}
	if v.Fingerprint != "chrome" {
		t.Fatalf("fp default = %q, want chrome", v.Fingerprint)
	}
	v, err = ParseVLESSURI("vless://uuid@10.0.0.1:8443")
	if err != nil {
		t.Fatal(err)
	}
	if v.Security != "none" {
		t.Fatalf("security default = %q, want none", v.Security)
	}
}

func TestParseVLESSURIErrors(t *testing.T) {
	cases := []string{
		"ss://uuid@host:443",                    // wrong scheme
		"vless://example.org:443",               // no uuid
		"vless://uuid@example.org",              // no port
		"vless://uuid@example.org:notaport",     // bad port
		"vless://uuid@example.org:443?type=ws",  // non-tcp transport
		"vless://uuid@example.org:443?security=tls",                // tls without sni
		"vless://uuid@example.org:443?security=reality",            // reality without pbk
		"vless://uuid@example.org:443?security=quic",               // unknown security
		"::::not a url::::::",
	}
	for _, uri := range cases {
		if _, err := ParseVLESSURI(uri); err == nil {
			t.Errorf("uri %q: expected error", uri)
		} else if !strings.Contains(err.Error(), "vless_uri") {
			t.Errorf("uri %q: error %v lacks context", uri, err)
		}
	}
}

func TestParseVLESSURITLS(t *testing.T) {
	v, err := ParseVLESSURI("vless://uuid@example.org:443?security=tls&sni=example.org")
	if err != nil {
		t.Fatal(err)
	}
	if v.Security != "tls" || v.SNI != "example.org" || v.Fingerprint != "chrome" {
		t.Fatalf("tls parse wrong: %+v", v)
	}
}
