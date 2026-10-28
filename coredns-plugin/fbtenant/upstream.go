package fbtenant

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Upstream schemes supported for tenant forward servers (CONTRACTS §2).
const (
	SchemeUDP   = "udp"
	SchemeTCP   = "tcp"
	SchemeTLS   = "tls"
	SchemeHTTPS = "https"
	SchemeH3    = "h3"
	SchemeQUIC  = "quic"
	SchemeSDNS  = "sdns"
)

var defaultPorts = map[string]int{
	SchemeUDP:   53,
	SchemeTCP:   53,
	SchemeTLS:   853,
	SchemeHTTPS: 443,
	SchemeH3:    443,
	SchemeQUIC:  853,
}

// UpstreamSpec is one parsed forward-server address. Accepted forms
// (addresses are examples):
//
//	94.140.14.140, [2a10:50c0::1:ff]            UDP :53
//	94.140.14.140:53, [2a10:50c0::1:ff]:53      UDP with port
//	udp://host[:port]                            UDP (hostname allowed)
//	tcp://ip|host[:port]                         TCP
//	tls://host[:853]                             DNS-over-TLS
//	https://host[:443][/dns-query]               DNS-over-HTTPS
//	h3://host[:443][/dns-query]                  DoH forced HTTP/3 (no h2 fallback)
//	quic://host[:853]                            DNS-over-QUIC (RFC 9250)
//	sdns://<base64url stamp>                     DNSCrypt or DoH stamp
type UpstreamSpec struct {
	Raw    string
	Scheme string
	// Host is a bare hostname or IP literal (no brackets).
	Host string
	Port int
	// Path is the DoH endpoint (https/h3), default "/dns-query".
	Path string
	// Stamp is the raw sdns:// payload for SchemeSDNS.
	Stamp string
	// HostIsName is true when Host is a DNS name, not an IP literal.
	HostIsName bool
}

// ParseUpstream parses one upstream address in any supported form.
func ParseUpstream(raw string) (*UpstreamSpec, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("empty upstream address")
	}

	if strings.HasPrefix(s, "sdns://") {
		stamp := strings.TrimPrefix(s, "sdns://")
		if stamp == "" {
			return nil, fmt.Errorf("sdns:// stamp is empty")
		}
		return &UpstreamSpec{Raw: s, Scheme: SchemeSDNS, Stamp: stamp}, nil
	}

	if i := strings.Index(s, "://"); i > 0 {
		scheme := strings.ToLower(s[:i])
		switch scheme {
		case SchemeUDP, SchemeTCP, SchemeTLS, SchemeHTTPS, SchemeH3, SchemeQUIC:
		default:
			return nil, fmt.Errorf("unsupported upstream scheme %q", scheme)
		}
		rest := s[i+3:]
		if rest == "" {
			return nil, fmt.Errorf("%s:// address is empty", scheme)
		}

		hostPart := rest
		path := ""
		if scheme == SchemeHTTPS || scheme == SchemeH3 {
			// path with query part is preserved for DoH
			if j := strings.IndexAny(rest, "/"); j >= 0 {
				hostPart, path = rest[:j], rest[j:]
			}
		} else if j := strings.Index(rest, "/"); j >= 0 {
			return nil, fmt.Errorf("%s:// upstream must not contain a path", scheme)
		}

		host, portStr, err := splitHostPort(hostPart)
		if err != nil {
			return nil, fmt.Errorf("%s://%s: %w", scheme, rest, err)
		}
		port := defaultPorts[scheme]
		if portStr != "" {
			p, err := strconv.Atoi(portStr)
			if err != nil || p < 1 || p > 65535 {
				return nil, fmt.Errorf("%s://%s: bad port %q", scheme, rest, portStr)
			}
			port = p
		}
		if path == "" && (scheme == SchemeHTTPS || scheme == SchemeH3) {
			path = "/dns-query"
		}
		return &UpstreamSpec{
			Raw: s, Scheme: scheme, Host: host, Port: port, Path: path,
			HostIsName: net.ParseIP(host) == nil,
		}, nil
	}

	// No scheme: plain "host[:port]" over UDP. A bare IPv6 literal keeps its
	// colons and must not be treated as host:port; a port requires the
	// bracketed form for IPv6.
	host, portStr, err := splitHostPort(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s, err)
	}
	port := defaultPorts[SchemeUDP]
	if portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil || p < 1 || p > 65535 {
			return nil, fmt.Errorf("%s: bad port %q", s, portStr)
		}
		port = p
	}
	return &UpstreamSpec{
		Raw: s, Scheme: SchemeUDP, Host: host, Port: port,
		HostIsName: net.ParseIP(host) == nil,
	}, nil
}

// splitHostPort splits "host", "host:port", "[v6]:port", "[v6]" into a bare
// host and an optional port, rejecting ambiguous forms.
func splitHostPort(s string) (host, port string, err error) {
	if s == "" {
		return "", "", fmt.Errorf("missing host")
	}
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
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
		if rest[1:] == "" {
			return "", "", fmt.Errorf("empty port")
		}
		return host, rest[1:], nil
	}
	if strings.Count(s, ":") > 1 {
		// bare IPv6 literal (no port)
		if net.ParseIP(s) == nil {
			return "", "", fmt.Errorf("ambiguous host %q (bracket IPv6 literals to add a port)", s)
		}
		return s, "", nil
	}
	h, p, splitErr := net.SplitHostPort(s)
	if splitErr != nil {
		// no port at all
		if net.ParseIP(s) == nil && !validHostname(s) {
			return "", "", fmt.Errorf("bad host %q", s)
		}
		return s, "", nil
	}
	if p == "" {
		return "", "", fmt.Errorf("empty port")
	}
	if net.ParseIP(h) == nil && !validHostname(h) {
		return "", "", fmt.Errorf("bad host %q", h)
	}
	return h, p, nil
}

func validHostname(s string) bool {
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

// Addr renders host:port in the canonical dialable form (brackets IPv6).
func (u *UpstreamSpec) Addr() string {
	if u.Scheme == SchemeSDNS {
		return "sdns://" + u.Stamp
	}
	return joinHostPort(u.Host, u.Port)
}

// URL renders the DoH endpoint for https/h3 schemes.
func (u *UpstreamSpec) URL() string {
	if u.Scheme != SchemeHTTPS && u.Scheme != SchemeH3 {
		return ""
	}
	return fmt.Sprintf("%s://%s%s", u.Scheme, joinHostPort(u.Host, u.Port), u.Path)
}

func joinHostPort(host string, port int) string {
	if strings.Contains(host, ":") {
		return net.JoinHostPort(host, strconv.Itoa(port))
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// Policy values for upstream selection (UCI upstream_policy).
const (
	PolicySequential = "sequential"
	PolicyRandom     = "random"
)

// NormalizePolicy maps free-form input to a valid policy, defaulting to
// sequential (the historical behaviour).
func NormalizePolicy(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), PolicyRandom) {
		return PolicyRandom
	}
	return PolicySequential
}
