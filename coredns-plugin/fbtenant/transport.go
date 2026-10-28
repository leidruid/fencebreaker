package fbtenant

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coredns/coredns/plugin/pkg/log"
	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/sys/unix"
)

// upstreamTransport exchanges one DNS message over one scheme.
type upstreamTransport interface {
	exchange(ctx context.Context, spec *UpstreamSpec, q *dns.Msg, mk marker) (*dns.Msg, error)
}

// marker carries the per-tenant SO_MARK for outgoing sockets.
type marker struct {
	mark    uint32
	enabled bool
}

func (m marker) control() func(string, string, syscall.RawConn) error {
	if !m.enabled || m.mark == 0 {
		return nil
	}
	return func(network, address string, conn syscall.RawConn) error {
		var sockErr error
		if err := conn.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(m.mark))
		}); err != nil {
			return err
		}
		return sockErr
	}
}

func (m marker) tlsConfig(serverName string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if serverName != "" && net.ParseIP(serverName) == nil {
		cfg.ServerName = serverName
	}
	return cfg
}

// hostCache resolves upstream hostnames (udp/tcp/tls DoT SNI aside) outside
// the tenant path: the bootstrap lookups deliberately use the system
// resolver unmarked.
type hostCache struct {
	mu      sync.Mutex
	entries map[string]hostEntry
	ttl     time.Duration
}

type hostEntry struct {
	ip     net.IP
	expiry time.Time
}

var hosts = &hostCache{entries: map[string]hostEntry{}, ttl: 60 * time.Second}

func (h *hostCache) resolve(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip, nil
	}
	h.mu.Lock()
	e, ok := h.entries[host]
	h.mu.Unlock()
	if ok && time.Now().Before(e.expiry) {
		return e.ip, nil
	}
	addrs, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("host %s has no addresses", host)
	}
	ip := addrs[0]
	h.mu.Lock()
	h.entries[host] = hostEntry{ip: ip, expiry: time.Now().Add(h.ttl)}
	h.mu.Unlock()
	return ip, nil
}

/*
	Plain DNS: udp, tcp and tls (DoT) via miekg/dns.
*/

type plainTransport struct {
	net string // udp | tcp | tcp-tls
}

func (t *plainTransport) exchange(ctx context.Context, spec *UpstreamSpec, q *dns.Msg, mk marker) (*dns.Msg, error) {
	timeout := transportTimeout(ctx)
	dialer := &net.Dialer{Timeout: timeout}
	if c := mk.control(); c != nil {
		dialer.Control = c
	}
	client := &dns.Client{
		Net:     t.net,
		Timeout: timeout,
		Dialer:  dialer,
		UDPSize: dns.DefaultMsgSize,
	}
	if t.net == "tcp-tls" {
		client.TLSConfig = mk.tlsConfig(spec.Host)
	}
	resp, _, err := client.ExchangeContext(ctx, q, spec.Addr())
	if err != nil {
		return nil, err
	}
	if resp.Truncated && t.net == "udp" {
		tcp := &plainTransport{net: "tcp"}
		return tcp.exchange(ctx, spec, q, mk)
	}
	return resp, nil
}

// transportTimeout picks the per-exchange timeout from ctx (cap 5s) so tests
// can tighten it.
func transportTimeout(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return time.Until(dl)
	}
	return 2 * time.Second
}

/*
	DNS-over-HTTPS: https:// (HTTP/2 or HTTP/3 negotiated by the stdlib) and
	h3:// (HTTP/3 only, no fallback to h2 or below).
*/

type dohTransport struct {
	forceH3 bool

	httpClient *http.Client
	h3Client   *http.Client
}

func newDoHTransport(forceH3 bool) *dohTransport {
	t := &dohTransport{forceH3: forceH3}
	t.httpClient = &http.Client{
		Transport: &http.Transport{
			ForceAttemptHTTP2: true,
		},
		Timeout: 5 * time.Second,
	}
	t.h3Client = &http.Client{
		Transport: &http3.Transport{
			// h3 only: the http3.Transport speaks QUIC exclusively and never
			// falls back to HTTP/2 or lower.
			Dial: t.dialH3,
		},
		Timeout: 5 * time.Second,
	}
	return t
}

// dialH3 dials QUIC with the SO_MARK applied to the UDP socket. The mark is
// taken from the request context via withMarker below.
func (t *dohTransport) dialH3(ctx context.Context, addrStr string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
	mk := markerFromContext(ctx)
	host, portStr, err := net.SplitHostPort(addrStr)
	if err != nil {
		return nil, err
	}
	ip, err := hosts.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	pc, err := listenMarkedUDP(ctx, ip, mk)
	if err != nil {
		return nil, err
	}
	tr := &quic.Transport{Conn: pc}
	return tr.Dial(ctx, &net.UDPAddr{IP: ip, Port: mustAtoi(portStr)}, tlsCfg, cfg)
}

type markerCtxKey struct{}

func withMarker(ctx context.Context, mk marker) context.Context {
	return context.WithValue(ctx, markerCtxKey{}, mk)
}

func markerFromContext(ctx context.Context) marker {
	if mk, ok := ctx.Value(markerCtxKey{}).(marker); ok {
		return mk
	}
	return marker{}
}

func (t *dohTransport) exchange(ctx context.Context, spec *UpstreamSpec, q *dns.Msg, mk marker) (*dns.Msg, error) {
	wire, err := q.Pack()
	if err != nil {
		return nil, err
	}
	ctx = withMarker(ctx, mk)
	client := t.httpClient
	url := spec.URL()
	if t.forceH3 {
		client = t.h3Client
		// http3.Transport only accepts https:// URLs; h3:// is our config
		// syntax meaning "https forced over HTTP/3, no fallback".
		url = "https://" + joinHostPort(spec.Host, spec.Port) + spec.Path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(wire)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("doh %s: HTTP %d", spec.URL(), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, dns.MaxMsgSize))
	if err != nil {
		return nil, err
	}
	m := new(dns.Msg)
	if err := m.Unpack(body); err != nil {
		return nil, fmt.Errorf("doh %s: bad response: %w", spec.URL(), err)
	}
	return m, nil
}

/*
	DNS-over-QUIC (RFC 9250): one long-lived QUIC connection, one stream per
	query, 2-byte length-prefixed wire messages, ALPN "doq".
*/

var doqALPN = []string{"doq"}

// doqDialQUIC is indirection for tests (extra root CAs on the local server).
var doqDialQUIC = func(ctx context.Context, pc *net.UDPConn, addr net.Addr, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
	return (&quic.Transport{Conn: pc}).Dial(ctx, addr, tlsCfg, cfg)
}

type doqTransport struct {
	mu    sync.Mutex
	conns map[string]*quic.Conn // spec.Addr() (+mark) -> conn
}

func newDoQTransport() *doqTransport {
	return &doqTransport{conns: map[string]*quic.Conn{}}
}

func (t *doqTransport) exchange(ctx context.Context, spec *UpstreamSpec, q *dns.Msg, mk marker) (*dns.Msg, error) {
	wire, err := q.Pack()
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%s|%d", spec.Addr(), mk.mark)

	tryOnce := func() (*dns.Msg, error) {
		conn, err := t.getConn(ctx, key, spec, mk)
		if err != nil {
			return nil, err
		}
		stream, err := conn.OpenStreamSync(ctx)
		if err != nil {
			return nil, err
		}
		defer stream.Close()
		var hdr [2]byte
		binary.BigEndian.PutUint16(hdr[:], uint16(len(wire)))
		if _, err := stream.Write(hdr[:]); err != nil {
			return nil, err
		}
		if _, err := stream.Write(wire); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(stream, hdr[:]); err != nil {
			return nil, err
		}
		n := binary.BigEndian.Uint16(hdr[:])
		if n == 0 || n > dns.MaxMsgSize {
			return nil, fmt.Errorf("doq %s: bad frame length %d", spec.Addr(), n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(stream, buf); err != nil {
			return nil, err
		}
		m := new(dns.Msg)
		if err := m.Unpack(buf); err != nil {
			return nil, fmt.Errorf("doq %s: bad response: %w", spec.Addr(), err)
		}
		return m, nil
	}

	m, err := tryOnce()
	if err != nil {
		// drop the (likely broken) connection and retry once with a fresh one
		t.dropConn(key)
		return tryOnce()
	}
	return m, nil
}

func (t *doqTransport) getConn(ctx context.Context, key string, spec *UpstreamSpec, mk marker) (*quic.Conn, error) {
	t.mu.Lock()
	conn := t.conns[key]
	t.mu.Unlock()
	if conn != nil {
		return conn, nil
	}
	ip, err := hosts.resolve(ctx, spec.Host)
	if err != nil {
		return nil, err
	}
	pc, err := listenMarkedUDP(ctx, ip, mk)
	if err != nil {
		return nil, err
	}
	tlsCfg := mk.tlsConfig(spec.Host)
	tlsCfg.NextProtos = doqALPN
	conn, err = doqDialQUIC(ctx, pc, &net.UDPAddr{IP: ip, Port: spec.Port}, tlsCfg, &quic.Config{
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
	})
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("doq %s: dial: %w", spec.Addr(), err)
	}
	t.mu.Lock()
	t.conns[key] = conn
	t.mu.Unlock()
	return conn, nil
}

func (t *doqTransport) dropConn(key string) {
	t.mu.Lock()
	conn := t.conns[key]
	delete(t.conns, key)
	t.mu.Unlock()
	if conn != nil {
		conn.CloseWithError(0, "recycle")
	}
}

// listenMarkedUDP opens a wildcard UDP socket suitable for reaching ip,
// with the tenant mark applied when enabled.
func listenMarkedUDP(ctx context.Context, ip net.IP, mk marker) (*net.UDPConn, error) {
	local := "0.0.0.0:0"
	if ip.To4() == nil {
		local = "[::]:0"
	}
	lc := net.ListenConfig{}
	if c := mk.control(); c != nil {
		lc.Control = c
	}
	pc, err := lc.ListenPacket(ctx, "udp", local)
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}

func mustAtoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// transportForScheme returns the shared transport for a scheme. sdns is
// served by the sdnsTransport (sdns.go).
func transportForScheme(scheme string) upstreamTransport {
	switch scheme {
	case SchemeUDP:
		return &plainTransport{net: "udp"}
	case SchemeTCP:
		return &plainTransport{net: "tcp"}
	case SchemeTLS:
		return &plainTransport{net: "tcp-tls"}
	case SchemeHTTPS:
		return transportHTTPS
	case SchemeH3:
		return transportH3
	case SchemeQUIC:
		return transportDOQ
	case SchemeSDNS:
		return transportSDNS
	}
	log.Warningf("fbtenant: no transport for scheme %q", scheme)
	return nil
}

var (
	transportHTTPS                   = newDoHTransport(false)
	transportH3                      = newDoHTransport(true)
	transportDOQ                     = newDoQTransport()
	transportSDNS  upstreamTransport = newSDNSTransport() // sdns.go
)
