package fbtenant

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ameshkov/dnscrypt"
	"github.com/ameshkov/dnsstamps"
	"github.com/miekg/dns"
)

/*
	sdns:// upstreams (CONTRACTS §2): the stamp payload is decoded with
	github.com/ameshkov/dnsstamps. DNSCrypt stamps are served by the
	ameshkov/dnscrypt client, DoH stamps are rewritten into an https
	UpstreamSpec and handed to the shared DoH transport.
*/

type sdnsTransport struct {
	mu      sync.Mutex
	servers map[string]*dnscrypt.ServerInfo // stamp string -> cert info
}

func newSDNSTransport() *sdnsTransport {
	return &sdnsTransport{servers: map[string]*dnscrypt.ServerInfo{}}
}

// exchange implements upstreamTransport.
func (t *sdnsTransport) exchange(ctx context.Context, spec *UpstreamSpec, q *dns.Msg, mk marker) (*dns.Msg, error) {
	stamp, err := parseStamp(spec.Stamp)
	if err != nil {
		return nil, fmt.Errorf("sdns %s: bad stamp: %w", spec.Stamp, err)
	}
	switch stamp.Proto {
	case dnsstamps.StampProtoTypeDNSCrypt:
		return t.exchangeDNSCrypt(ctx, spec, stamp, q, mk)
	case dnsstamps.StampProtoTypeDoH:
		return transportHTTPS.exchange(ctx, dohSpecFromStamp(stamp), q, mk)
	default:
		return nil, fmt.Errorf("sdns %s: stamp protocol %d not supported", spec.Stamp, stamp.Proto)
	}
}

// parseStamp decodes the payload of an sdns:// address (with or without the
// leading scheme) into a dnsstamps ServerStamp.
func parseStamp(stamp string) (*dnsstamps.ServerStamp, error) {
	s, err := dnsstamps.NewServerStampFromString("sdns://" + strings.TrimPrefix(stamp, "sdns://"))
	if err != nil {
		return nil, err
	}
	return &s, nil
}

/*
	DNSCrypt stamps: the server certificate is fetched once via
	Client.DialStamp and cached until it expires; a failed exchange usually
	means the certificate was rotated, so the cache is dropped and the server
	is re-dialed once.
*/

func (t *sdnsTransport) exchangeDNSCrypt(ctx context.Context, spec *UpstreamSpec, stamp *dnsstamps.ServerStamp, q *dns.Msg, mk marker) (*dns.Msg, error) {
	// dnscrypt.Client has no ctx support, so the effective deadline is
	// tracked here and every dial/read gets only what is left of it.
	deadline := time.Now().Add(transportTimeout(ctx))

	server, err := t.server(spec.Stamp, stamp, positiveTimeout(time.Until(deadline)))
	if err != nil {
		return nil, fmt.Errorf("sdns %s: dial: %w", spec.Stamp, err)
	}
	m, err := exchangeDNSCryptOnce(ctx, q, server, positiveTimeout(time.Until(deadline)), mk)
	if err == nil {
		return m, nil
	}

	// Stale ServerInfo (rotated certificate): drop it, re-dial once and
	// retry while the ctx budget lasts.
	t.invalidate(spec.Stamp)
	if time.Until(deadline) <= 0 {
		return nil, fmt.Errorf("sdns %s: %w", spec.Stamp, err)
	}
	left := positiveTimeout(time.Until(deadline))
	server, dialErr := t.server(spec.Stamp, stamp, left)
	if dialErr != nil {
		return nil, fmt.Errorf("sdns %s: %w (re-dial: %v)", spec.Stamp, err, dialErr)
	}
	m, retryErr := exchangeDNSCryptOnce(ctx, q, server, left, mk)
	if retryErr != nil {
		t.invalidate(spec.Stamp)
		return nil, fmt.Errorf("sdns %s: %w", spec.Stamp, retryErr)
	}
	return m, nil
}

// exchangeDNSCryptOnce encrypts and sends one query over a fresh UDP socket
// dialed with the tenant mark applied (the stock Client.Exchange cannot
// mark its sockets).
func exchangeDNSCryptOnce(ctx context.Context, q *dns.Msg, server *dnscrypt.ServerInfo, timeout time.Duration, mk marker) (*dns.Msg, error) {
	dialer := &net.Dialer{Timeout: timeout}
	if c := mk.control(); c != nil {
		dialer.Control = c
	}
	conn, err := dialer.DialContext(ctx, "udp", server.ServerAddress)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	client := &dnscrypt.Client{Proto: "udp", Timeout: timeout, AdjustPayloadSize: true}
	m, _, err := client.ExchangeConn(q, server, conn)
	return m, err
}

// server returns the cached ServerInfo for the stamp, dialing for a fresh
// certificate when there is none or the cached one has expired.
func (t *sdnsTransport) server(key string, stamp *dnsstamps.ServerStamp, timeout time.Duration) (*dnscrypt.ServerInfo, error) {
	t.mu.Lock()
	si := t.servers[key]
	t.mu.Unlock()
	if si != nil && !certExpired(si) {
		return si, nil
	}
	client := &dnscrypt.Client{Proto: "udp", Timeout: timeout}
	si, _, err := client.DialStamp(*stamp)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.servers[key] = si
	t.mu.Unlock()
	return si, nil
}

// invalidate drops the cached ServerInfo for the stamp.
func (t *sdnsTransport) invalidate(key string) {
	t.mu.Lock()
	delete(t.servers, key)
	t.mu.Unlock()
}

// certExpired reports whether the server certificate validity window
// (CertInfo.NotBefore/NotAfter, epoch seconds) has passed.
func certExpired(si *dnscrypt.ServerInfo) bool {
	if si.ServerCert == nil {
		return true
	}
	now := uint32(time.Now().Unix())
	return now >= si.ServerCert.NotAfter || now < si.ServerCert.NotBefore
}

// positiveTimeout clamps a leftover ctx budget to a minimal positive value:
// a zero Client.Timeout would mean "infinite" for the dnscrypt client.
func positiveTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Millisecond
	}
	return d
}

/*
	DoH stamps: rewritten into a plain https UpstreamSpec and delegated to
	the shared DoH transport (transportHTTPS, never transportForScheme, to
	avoid recursion).
*/

// dohSpecFromStamp converts a DoH stamp into an https UpstreamSpec.
// ServerAddrStr may be "host", "host:port", "[v6]" or "[v6]:port"; the port
// defaults to 443 and the path to "/dns-query". Stamps with an empty
// address fall back to the provider hostname.
func dohSpecFromStamp(stamp *dnsstamps.ServerStamp) *UpstreamSpec {
	host, portStr := splitStampAddr(stamp.ServerAddrStr)
	if host == "" {
		host, _ = splitStampAddr(stamp.ProviderName)
	}
	port := defaultPorts[SchemeHTTPS]
	if p, err := strconv.Atoi(portStr); err == nil && p > 0 && p < 65536 {
		port = p
	}
	path := stamp.Path
	if path == "" {
		path = "/dns-query"
	}
	return &UpstreamSpec{
		Raw:        "https://" + joinHostPort(host, port) + path,
		Scheme:     SchemeHTTPS,
		Host:       host,
		Port:       port,
		Path:       path,
		HostIsName: net.ParseIP(host) == nil,
	}
}

// splitStampAddr splits a stamp ServerAddrStr into a bare host and an
// optional port ("host", "host:port", "[v6]", "[v6]:port").
func splitStampAddr(addr string) (host, port string) {
	if strings.HasPrefix(addr, "[") {
		if end := strings.Index(addr, "]"); end > 0 {
			rest := addr[end+1:]
			if strings.HasPrefix(rest, ":") {
				return addr[1:end], rest[1:]
			}
			return addr[1:end], ""
		}
	}
	if h, p, err := net.SplitHostPort(addr); err == nil {
		return h, p
	}
	return addr, ""
}
