package fbtenant

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/ameshkov/dnscrypt"
	"github.com/ameshkov/dnsstamps"
	"github.com/miekg/dns"
)

// Stamp payloads verified against the github.com/ameshkov/dnsstamps v1.0.1
// test suite.
const (
	// AdGuard DNSCrypt: 176.103.130.130:5443, provider
	// 2.dnscrypt.default.ns1.adguard.com.
	adguardDNSCryptStamp = "AQIAAAAAAAAAFDE3Ni4xMDMuMTMwLjEzMDo1NDQzINErR_JS3PLCu_iZEIbq95zkSV2LFsigxDIuUso_OQhzIjIuZG5zY3J5cHQuZGVmYXVsdC5uczEuYWRndWFyZC5jb20"
	// Google DoH: empty address (falls back to the provider name),
	// dns.google.com, path /experimental.
	googleDoHStamp = "AgUAAAAAAAAAACAe9iTP_15r07rd8_3b_epWVGfjdymdx-5mdRZvMAzBuQ5kbnMuZ29vZ2xlLmNvbQ0vZXhwZXJpbWVudGFs"
	// Local DoH: 127.0.0.1 (parser appends the default :443), provider
	// example.com, path /dns-query.
	localDoHStamp = "AgcAAAAAAAAACTEyNy4wLjAuMSDDhGvyS56TymQnTA7GfB7MXgJP_KzS10AZNQ6B_lRq5AtleGFtcGxlLmNvbQovZG5zLXF1ZXJ5"
	// Plain DNS 8.8.8.8 (proto 0x00) and DoT dns.adguard.com (proto 0x03).
	plainStamp = "AAcAAAAAAAAABzguOC44Ljg"
	dotStamp   = "AwAAAAAAAAAAAAAPZG5zLmFkZ3VhcmQuY29t"
)

// buildStamp encodes a stamp by hand: proto(1) props(8, LE) followed by
// LP-strings (1 length byte + bytes). For DoH the fields are addr, hashes
// ("" = the single 0x00 "no hashes" byte), provider, path; for DNSCrypt
// addr, pk, provider; for plain just addr.
func buildStamp(proto byte, fields ...string) string {
	bin := []byte{proto}
	var props [8]byte
	binary.LittleEndian.PutUint64(props[:], 0)
	bin = append(bin, props[:]...)
	for _, f := range fields {
		bin = append(bin, uint8(len(f)))
		bin = append(bin, f...)
	}
	return base64.RawURLEncoding.EncodeToString(bin)
}

// localhostDNSCryptStamp points at a closed local port, so certificate
// fetches fail fast without leaving the machine.
func localhostDNSCryptStamp(t *testing.T) *dnsstamps.ServerStamp {
	t.Helper()
	pk := string(make([]byte, 32))
	stamp, err := parseStamp(buildStamp(0x01, "127.0.0.1:1", pk, "2.dnscrypt-cert.example"))
	if err != nil {
		t.Fatalf("parseStamp: %v", err)
	}
	if stamp.Proto != dnsstamps.StampProtoTypeDNSCrypt {
		t.Fatalf("proto = %d, want DNSCrypt", stamp.Proto)
	}
	return stamp
}

// ---- parseStamp ------------------------------------------------------------

func TestSDNSParseStampValid(t *testing.T) {
	tests := []struct {
		name       string
		stamp      string
		proto      dnsstamps.StampProtoType
		serverAddr string
		provider   string
		path       string
		pkLen      int
	}{
		{
			name:       "dnscrypt adguard",
			stamp:      adguardDNSCryptStamp,
			proto:      dnsstamps.StampProtoTypeDNSCrypt,
			serverAddr: "176.103.130.130:5443",
			provider:   "2.dnscrypt.default.ns1.adguard.com",
			pkLen:      32,
		},
		{
			name:     "doh google",
			stamp:    googleDoHStamp,
			proto:    dnsstamps.StampProtoTypeDoH,
			provider: "dns.google.com",
			path:     "/experimental",
		},
		{
			name:       "doh local gets default port",
			stamp:      localDoHStamp,
			proto:      dnsstamps.StampProtoTypeDoH,
			serverAddr: "127.0.0.1:443",
			provider:   "example.com",
			path:       "/dns-query",
		},
		{
			name:       "doh hostname with port",
			stamp:      buildStamp(0x02, "dns.example.com:8443", "", "example.com", "/custom"),
			proto:      dnsstamps.StampProtoTypeDoH,
			serverAddr: "dns.example.com:8443",
			provider:   "example.com",
			path:       "/custom",
		},
		{
			name:       "doh bracketed v6 gets default port",
			stamp:      buildStamp(0x02, "[2001:db8::1]", "", "example.com", "/dns-query"),
			proto:      dnsstamps.StampProtoTypeDoH,
			serverAddr: "[2001:db8::1]:443",
			provider:   "example.com",
			path:       "/dns-query",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stamp, err := parseStamp(tt.stamp)
			if err != nil {
				t.Fatalf("parseStamp(%q): %v", tt.stamp, err)
			}
			if stamp.Proto != tt.proto {
				t.Fatalf("proto = %d, want %d", stamp.Proto, tt.proto)
			}
			if stamp.ServerAddrStr != tt.serverAddr {
				t.Fatalf("serverAddr = %q, want %q", stamp.ServerAddrStr, tt.serverAddr)
			}
			if stamp.ProviderName != tt.provider {
				t.Fatalf("provider = %q, want %q", stamp.ProviderName, tt.provider)
			}
			if stamp.Path != tt.path {
				t.Fatalf("path = %q, want %q", stamp.Path, tt.path)
			}
			if len(stamp.ServerPk) != tt.pkLen {
				t.Fatalf("pk length = %d, want %d", len(stamp.ServerPk), tt.pkLen)
			}
		})
	}
}

func TestSDNSParseStampAcceptsFullSchemeForm(t *testing.T) {
	with, err := parseStamp("sdns://" + localDoHStamp)
	if err != nil {
		t.Fatalf("parseStamp with scheme: %v", err)
	}
	without, err := parseStamp(localDoHStamp)
	if err != nil {
		t.Fatalf("parseStamp without scheme: %v", err)
	}
	if with.Proto != without.Proto || with.ServerAddrStr != without.ServerAddrStr ||
		with.ProviderName != without.ProviderName || with.Path != without.Path {
		t.Fatalf("scheme form mismatch: %+v vs %+v", with, without)
	}
}

func TestSDNSParseStampErrors(t *testing.T) {
	tests := []struct {
		name  string
		stamp string
	}{
		{name: "empty", stamp: ""},
		{name: "scheme only", stamp: "sdns://"},
		{name: "not base64", stamp: "!!!not-base64!!!"},
		{name: "dnscrypt too short", stamp: base64.RawURLEncoding.EncodeToString([]byte{0x01})},
		{name: "doh too short", stamp: base64.RawURLEncoding.EncodeToString([]byte{0x02})},
		{name: "doh lp string overruns", stamp: base64.RawURLEncoding.EncodeToString(
			append([]byte{0x02, 0, 0, 0, 0, 0, 0, 0, 0, 5, 'a', 'b'}, 0))},
		{name: "unknown proto", stamp: base64.RawURLEncoding.EncodeToString(
			append([]byte{0x7f, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 'x'))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseStamp(tt.stamp); err == nil {
				t.Fatalf("parseStamp(%q) succeeded, want error", tt.stamp)
			}
		})
	}
}

// ---- exchange: stamp dispatch without network -------------------------------

func TestSDNSExchangeBadStamp(t *testing.T) {
	spec, err := ParseUpstream("sdns://!!!not-base64!!!")
	if err != nil {
		t.Fatalf("ParseUpstream: %v", err)
	}
	tr := newSDNSTransport()
	q := new(dns.Msg).SetQuestion("example.com.", dns.TypeA)
	_, err = tr.exchange(context.Background(), spec, q, marker{})
	if err == nil || !strings.Contains(err.Error(), "bad stamp") {
		t.Fatalf("err = %v, want bad stamp error", err)
	}
}

func TestSDNSExchangeUnsupportedProto(t *testing.T) {
	tests := []struct {
		name  string
		stamp string
		proto string
	}{
		{name: "plain", stamp: plainStamp, proto: "0"},
		{name: "tls", stamp: dotStamp, proto: "3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := ParseUpstream("sdns://" + tt.stamp)
			if err != nil {
				t.Fatalf("ParseUpstream: %v", err)
			}
			tr := newSDNSTransport()
			q := new(dns.Msg).SetQuestion("example.com.", dns.TypeA)
			_, err = tr.exchange(context.Background(), spec, q, marker{})
			if err == nil || !strings.Contains(err.Error(), "not supported") ||
				!strings.Contains(err.Error(), tt.proto) {
				t.Fatalf("err = %v, want stamp protocol %s not supported", err, tt.proto)
			}
		})
	}
}

// ---- dohSpecFromStamp -------------------------------------------------------

func TestSDNSDoHSpecFromStamp(t *testing.T) {
	tests := []struct {
		name   string
		stamp  string
		host   string
		port   int
		isName bool
		path   string
		url    string
	}{
		{
			name:   "google empty address falls back to provider",
			stamp:  googleDoHStamp,
			host:   "dns.google.com",
			port:   443,
			isName: true,
			path:   "/experimental",
			url:    "https://dns.google.com:443/experimental",
		},
		{
			name:   "local ip with default port and path",
			stamp:  localDoHStamp,
			host:   "127.0.0.1",
			port:   443,
			isName: false,
			path:   "/dns-query",
			url:    "https://127.0.0.1:443/dns-query",
		},
		{
			name:   "hostname with port and custom path",
			stamp:  buildStamp(0x02, "dns.example.com:8443", "", "example.com", "/custom"),
			host:   "dns.example.com",
			port:   8443,
			isName: true,
			path:   "/custom",
			url:    "https://dns.example.com:8443/custom",
		},
		{
			name:   "bracketed v6 default port",
			stamp:  buildStamp(0x02, "[2001:db8::1]", "", "example.com", "/dns-query"),
			host:   "2001:db8::1",
			port:   443,
			isName: false,
			path:   "/dns-query",
			url:    "https://[2001:db8::1]:443/dns-query",
		},
		{
			name:   "bracketed v6 with port",
			stamp:  buildStamp(0x02, "[2001:db8::1]:8443", "", "example.com", "/dns-query"),
			host:   "2001:db8::1",
			port:   8443,
			isName: false,
			path:   "/dns-query",
			url:    "https://[2001:db8::1]:8443/dns-query",
		},
		{
			name:   "empty path defaults to /dns-query",
			stamp:  buildStamp(0x02, "127.0.0.1", "", "example.com", ""),
			host:   "127.0.0.1",
			port:   443,
			isName: false,
			path:   "/dns-query",
			url:    "https://127.0.0.1:443/dns-query",
		},
		{
			name:   "bogus port falls back to 443",
			stamp:  buildStamp(0x02, "example.com:notaport", "", "example.com", "/dns-query"),
			host:   "example.com",
			port:   443,
			isName: true,
			path:   "/dns-query",
			url:    "https://example.com:443/dns-query",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stamp, err := parseStamp(tt.stamp)
			if err != nil {
				t.Fatalf("parseStamp: %v", err)
			}
			spec := dohSpecFromStamp(stamp)
			if spec.Scheme != SchemeHTTPS {
				t.Fatalf("scheme = %q, want https", spec.Scheme)
			}
			if spec.Host != tt.host {
				t.Fatalf("host = %q, want %q", spec.Host, tt.host)
			}
			if spec.Port != tt.port {
				t.Fatalf("port = %d, want %d", spec.Port, tt.port)
			}
			if spec.HostIsName != tt.isName {
				t.Fatalf("hostIsName = %v, want %v", spec.HostIsName, tt.isName)
			}
			if spec.Path != tt.path {
				t.Fatalf("path = %q, want %q", spec.Path, tt.path)
			}
			if got := spec.URL(); got != tt.url {
				t.Fatalf("url = %q, want %q", got, tt.url)
			}
		})
	}
}

// ---- DNSCrypt cache ---------------------------------------------------------

func TestSDNSCertExpired(t *testing.T) {
	now := time.Now()
	if !certExpired(&dnscrypt.ServerInfo{}) {
		t.Fatal("nil cert should count as expired")
	}
	past := &dnscrypt.ServerInfo{ServerCert: &dnscrypt.CertInfo{
		NotBefore: uint32(now.Add(-2 * time.Hour).Unix()),
		NotAfter:  uint32(now.Add(-time.Hour).Unix()),
	}}
	if !certExpired(past) {
		t.Fatal("past window should count as expired")
	}
	notYet := &dnscrypt.ServerInfo{ServerCert: &dnscrypt.CertInfo{
		NotBefore: uint32(now.Add(time.Hour).Unix()),
		NotAfter:  uint32(now.Add(2 * time.Hour).Unix()),
	}}
	if !certExpired(notYet) {
		t.Fatal("future window should count as expired")
	}
	valid := &dnscrypt.ServerInfo{ServerCert: &dnscrypt.CertInfo{
		NotBefore: uint32(now.Add(-time.Hour).Unix()),
		NotAfter:  uint32(now.Add(time.Hour).Unix()),
	}}
	if certExpired(valid) {
		t.Fatal("live window should not count as expired")
	}
}

func TestSDNSServerCache(t *testing.T) {
	tr := newSDNSTransport()
	stamp := localhostDNSCryptStamp(t)

	// Cache hit: a live ServerInfo is returned as-is, no network involved.
	live := &dnscrypt.ServerInfo{
		ServerAddress: "127.0.0.1:1",
		ProviderName:  "2.dnscrypt-cert.example.",
		ServerCert: &dnscrypt.CertInfo{
			NotBefore: uint32(time.Now().Add(-time.Hour).Unix()),
			NotAfter:  uint32(time.Now().Add(time.Hour).Unix()),
		},
	}
	tr.servers[stamp.String()] = live
	got, err := tr.server(stamp.String(), stamp, time.Second)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if got != live {
		t.Fatal("cached ServerInfo not reused")
	}

	// Cache miss / expired entry: the re-dial against the closed local port
	// must fail rather than serve the stale entry.
	expired := &dnscrypt.ServerInfo{ServerCert: &dnscrypt.CertInfo{
		NotBefore: uint32(time.Now().Add(-2 * time.Hour).Unix()),
		NotAfter:  uint32(time.Now().Add(-time.Hour).Unix()),
	}}
	tr.servers[stamp.String()] = expired
	if _, err := tr.server(stamp.String(), stamp, 50*time.Millisecond); err == nil {
		t.Fatal("expected re-dial error for expired cert entry")
	}

	// invalidate drops the entry entirely.
	tr.servers[stamp.String()] = live
	tr.invalidate(stamp.String())
	tr.mu.Lock()
	_, cached := tr.servers[stamp.String()]
	tr.mu.Unlock()
	if cached {
		t.Fatal("invalidate did not drop the cached entry")
	}
}
