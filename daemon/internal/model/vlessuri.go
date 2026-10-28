package model

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ParseVLESSURI converts a vless:// share link into the sing-box outbound
// parameters. Only the TCP transport (with TLS/REALITY) is supported in v1,
// matching the per-field UCI options it supersedes; the fragment (#name) is
// ignored. When the link carries no security the result defaults to "none".
func ParseVLESSURI(uri string) (*VLESS, error) {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return nil, fmt.Errorf("vless_uri: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "vless") {
		return nil, fmt.Errorf("vless_uri: scheme must be vless://")
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, fmt.Errorf("vless_uri: missing uuid")
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("vless_uri: missing server")
	}
	if u.Port() == "" {
		return nil, fmt.Errorf("vless_uri: missing port")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("vless_uri: bad port %q", u.Port())
	}

	q := u.Query()
	if t := strings.ToLower(q.Get("type")); t != "" && t != "tcp" {
		return nil, fmt.Errorf("vless_uri: transport %q not supported (TCP only in v1)", q.Get("type"))
	}

	v := &VLESS{
		UUID:        u.User.Username(),
		Server:      host,
		Port:        port,
		Flow:        q.Get("flow"),
		Security:    strings.ToLower(q.Get("security")),
		SNI:         q.Get("sni"),
		Fingerprint: q.Get("fp"),
		PublicKey:   q.Get("pbk"),
		ShortID:     q.Get("sid"),
	}

	switch v.Security {
	case "":
		v.Security = "none"
	case "reality":
		if v.PublicKey == "" {
			return nil, fmt.Errorf("vless_uri: reality requires pbk (public key)")
		}
	case "tls":
		if v.SNI == "" {
			return nil, fmt.Errorf("vless_uri: tls requires sni")
		}
	case "none":
	default:
		return nil, fmt.Errorf("vless_uri: unknown security %q", v.Security)
	}

	if v.Security != "none" && v.Fingerprint == "" {
		v.Fingerprint = "chrome"
	}

	return v, nil
}
