package model

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ValidateDNSUpstreamAddr checks the syntax of one dns_upstream entry in any
// of the forms understood by the fbtenant CoreDNS plugin (CONTRACTS §2):
//
//	94.140.14.140, [2a10:50c0::1:ff]         UDP :53
//	94.140.14.140:53, [2a10:50c0::1:ff]:53   UDP with port
//	udp://host[:port]                         UDP (hostname allowed)
//	tcp://ip|host[:port]                      TCP
//	tls://host[:853]                          DNS-over-TLS
//	https://host[:443][/dns-query]            DNS-over-HTTPS
//	h3://host[:443][/dns-query]               DoH forced HTTP/3 (no h2 fallback)
//	quic://host[:853]                         DNS-over-QUIC (RFC 9250)
//	sdns://<base64url stamp>                  DNSCrypt or DoH stamp
//
// Only the syntax is validated: ports must be 1..65535, the scheme must be
// one of the above (case-insensitive, like the plugin), https/h3 may carry a
// path starting with "/", bare IPv6 literals are valid without a port but
// require brackets with one, and hostnames must be valid DNS names.
// Reachability and name resolution are left to the plugin.
func ValidateDNSUpstreamAddr(s string) error {
	a := strings.TrimSpace(s)
	if a == "" {
		return fmt.Errorf("empty address")
	}

	if strings.HasPrefix(a, "sdns://") {
		return validateStamp(strings.TrimPrefix(a, "sdns://"))
	}

	if i := strings.Index(a, "://"); i > 0 {
		scheme := strings.ToLower(a[:i])
		switch scheme {
		case "udp", "tcp", "tls", "https", "h3", "quic":
		default:
			return fmt.Errorf("unsupported scheme %q (want udp, tcp, tls, https, h3, quic or sdns)", scheme)
		}
		rest := a[i+3:]
		if rest == "" {
			return fmt.Errorf("%s:// address is empty", scheme)
		}
		hostPart := rest
		if scheme == "https" || scheme == "h3" {
			// a DoH endpoint path is allowed and must start with "/"
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				if j == 0 {
					return fmt.Errorf("%s:// address is missing a host", scheme)
				}
				if strings.ContainsAny(rest[j:], " \t") {
					return fmt.Errorf("%s:// path %q contains spaces", scheme, rest[j:])
				}
				hostPart = rest[:j]
			}
		} else if strings.IndexByte(rest, '/') >= 0 {
			return fmt.Errorf("%s:// upstream must not contain a path", scheme)
		}
		if err := validateAuthority(hostPart); err != nil {
			return fmt.Errorf("%s://%s: %w", scheme, rest, err)
		}
		return nil
	}

	// No scheme: plain host[:port] over UDP.
	if err := validateAuthority(a); err != nil {
		return fmt.Errorf("%s: %w", a, err)
	}
	return nil
}

// validateAuthority checks the "host[:port]" part shared by every form.
func validateAuthority(auth string) error {
	if auth == "" {
		return fmt.Errorf("missing host")
	}
	_, port, err := splitUpstreamHostPort(auth)
	if err != nil {
		return err
	}
	if port == "" {
		return nil
	}
	return checkUpstreamPort(port)
}

// checkUpstreamPort verifies a decimal port in 1..65535.
func checkUpstreamPort(port string) error {
	if port == "" || !isDigits(port) {
		return fmt.Errorf("bad port %q", port)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("bad port %q (want 1..65535)", port)
	}
	return nil
}

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// splitUpstreamHostPort splits "host", "host:port", "[v6]:port" and "[v6]"
// into a bare host and an optional port, rejecting ambiguous forms such as
// "host:53:54" or an unbracketed IPv6 literal with a port. The host must be
// an IP literal or a valid DNS name.
func splitUpstreamHostPort(s string) (host, port string, err error) {
	if s == "" {
		return "", "", fmt.Errorf("missing host")
	}
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return "", "", fmt.Errorf("missing ] in bracketed host")
		}
		host = s[1:end]
		if net.ParseIP(host) == nil {
			return "", "", fmt.Errorf("bracketed host %q is not an IP literal", host)
		}
		rest := s[end+1:]
		if rest == "" {
			return host, "", nil
		}
		if !strings.HasPrefix(rest, ":") {
			return "", "", fmt.Errorf("unexpected %q after bracketed host", rest)
		}
		return host, rest[1:], nil
	}
	if strings.Count(s, ":") > 1 {
		// bare IPv6 literal; a port would require brackets
		if net.ParseIP(s) == nil {
			return "", "", fmt.Errorf("ambiguous host %q (bracket IPv6 literals to add a port)", s)
		}
		return s, "", nil
	}
	h, p, splitErr := net.SplitHostPort(s)
	if splitErr != nil {
		// no port at all
		if net.ParseIP(s) == nil && !validUpstreamHostname(s) {
			return "", "", fmt.Errorf("bad host %q", s)
		}
		return s, "", nil
	}
	if net.ParseIP(h) == nil && !validUpstreamHostname(h) {
		return "", "", fmt.Errorf("bad host %q", h)
	}
	return h, p, nil
}

// validUpstreamHostname reports whether s is a valid DNS name: labels of
// letters/digits/hyphens (hyphen not at the edges), 1..63 chars each, total
// up to 253.
func validUpstreamHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
			if !ok || (i == 0 || i == len(label)-1) && c == '-' {
				return false
			}
		}
	}
	return true
}

// validateStamp checks the sdns:// payload: a non-empty base64url body
// ([A-Za-z0-9_-]) with at most the standard two padding characters.
func validateStamp(stamp string) error {
	body := strings.TrimRight(stamp, "=")
	if body == "" {
		return fmt.Errorf("sdns:// stamp is empty")
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
		if !ok {
			return fmt.Errorf("sdns:// stamp %q is not base64url", stamp)
		}
	}
	if len(stamp)-len(body) > 2 {
		return fmt.Errorf("sdns:// stamp %q has too much padding", stamp)
	}
	return nil
}
