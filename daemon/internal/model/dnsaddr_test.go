package model

import "testing"

// One valid entry per supported upstream form (v4 / v6 / hostname variants).
func TestValidateDNSUpstreamAddrValid(t *testing.T) {
	valid := []string{
		// 1. bare address, UDP :53
		"94.140.14.140",
		"[2a10:50c0::1:ff]",
		// 2. bare address with port
		"94.140.14.140:53",
		"[2a10:50c0::1:ff]:53",
		// 3. udp:// by name / IP
		"udp://dns.example.com",
		"udp://1.2.3.4:5353",
		"udp://[2a10:50c0::1:ff]",
		// 4. tcp://, default port
		"tcp://1.2.3.4",
		"tcp://[2a10:50c0::1:ff]",
		// 5. tcp:// with port
		"tcp://1.2.3.4:53",
		"tcp://[2a10:50c0::1:ff]:53",
		// 6. tcp:// by name
		"tcp://dns.example.com",
		"tcp://dns.example.com:5353",
		// 7. tls:// DNS-over-TLS
		"tls://dns.example.com",
		"tls://1.2.3.4:853",
		"tls://[2a10:50c0::1:ff]:853",
		// 8. https:// DoH
		"https://dns.example.com",
		"https://dns.example.com/dns-query",
		"https://1.2.3.4:443/dns-query",
		// 9. h3:// DoH, HTTP/3 only
		"h3://dns.example.com",
		"h3://dns.example.com:8443/custom-path",
		"h3://[2a10:50c0::1:ff]",
		// 10. quic:// DNS-over-QUIC
		"quic://dns.example.com",
		"quic://1.2.3.4:853",
		"quic://[2a10:50c0::1:ff]:853",
		// 11. sdns:// DNS Stamp
		"sdns://AgcAAAAAAAAABzEuMC4wLjGgENk8hgpZb_zjVRM6Lm5j1vF3FQ",
		"sdns://AQcAAAAAAAAABzEuMC4wLjE",
		// scheme case-insensitivity (like the plugin)
		"TCP://1.2.3.4",
	}
	for _, s := range valid {
		if err := ValidateDNSUpstreamAddr(s); err != nil {
			t.Errorf("ValidateDNSUpstreamAddr(%q) = %v, want nil", s, err)
		}
	}
}

func TestValidateDNSUpstreamAddrInvalid(t *testing.T) {
	invalid := []string{
		"",
		"http://x",             // scheme outside the supported list
		"tcp://host/path",      // path on a non-DoH scheme
		"udp://host:notaport",  // non-numeric port
		"94.140.14.140:99999",  // port out of range
		"[bad",                 // unterminated bracket
		"sdns://",              // empty stamp
		"host:53:54",           // ambiguous host:port:port
		"sdns://!!!not-base64", // stamp charset
		"sdns://c2h>",          // non-base64url character
		"udp://",               // empty address after scheme
		"https:///dns-query",   // host missing
		":53",                  // host missing, bare form
		"94.140.14.140:0",      // port 0
		"bad host",             // space in host
		"-bad.host-",           // bad DNS labels
		"[not-an-ip]:53",       // bracketed host must be an IP literal
		"1:2:3:4:5:6:7:8:9",    // not a valid IPv6, too many groups
	}
	for _, s := range invalid {
		if err := ValidateDNSUpstreamAddr(s); err == nil {
			t.Errorf("ValidateDNSUpstreamAddr(%q) = nil, want error", s)
		}
	}
}
