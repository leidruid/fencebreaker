package fbtenant

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/coredns/coredns/plugin/pkg/log"
	"github.com/miekg/dns"
	"golang.org/x/sys/unix"
)

// upstreamClient resolves queries against configured upstream servers.
type upstreamClient interface {
	// Resolve sends qname/qtype through the upstream pool: servers are
	// ordered by policy, unhealthy ones are deprioritized, failures mark
	// the upstream unusable with a doubling penalty. mark > 0 requests
	// SO_MARK on outgoing sockets when supported.
	Resolve(ctx context.Context, servers []string, policy string, qname string, qtype uint16, mark uint32) (*dns.Msg, error)
}

const upstreamTimeout = 2 * time.Second

// healthViewer is implemented by clients that can report upstream health.
type healthViewer interface {
	UpstreamHealth(addrs []string) []UpstreamHealthInfo
}

// dnsUpstreamClient is the default upstreamClient over the scheme
// transports (transport.go).
type dnsUpstreamClient struct {
	// timeout per exchange attempt.
	timeout time.Duration
	// markSupported caches the SO_MARK capability probe; nil until probed.
	markSupported *bool

	health *upstreamHealth
	// specs caches parsed addresses across queries.
	specs sync.Map
	// randFn is injectable for tests (nil: math/rand).
	randFn func(n int) int
}

func newDNSUpstreamClient() *dnsUpstreamClient {
	return &dnsUpstreamClient{
		timeout: upstreamTimeout,
		health:  newUpstreamHealth(),
	}
}

// UpstreamHealth implements healthViewer. addrs are the raw configured
// addresses; they are resolved through the spec cache so the health keys
// (canonical host:port) match the ones updated during exchanges.
func (c *dnsUpstreamClient) UpstreamHealth(addrs []string) []UpstreamHealthInfo {
	keys := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if sp := c.spec(a); sp != nil {
			keys = append(keys, sp.Addr())
		} else {
			keys = append(keys, a)
		}
	}
	return c.health.snapshot(keys, time.Now())
}

// probeMarkCapability checks once whether this process may set SO_MARK
// (requires CAP_NET_ADMIN). On failure marking is disabled for the lifetime
// of the process and a log line is emitted.
func (c *dnsUpstreamClient) probeMarkCapability() bool {
	if c.markSupported != nil {
		return *c.markSupported
	}
	ok := false
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err == nil {
		err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_MARK, 0x1000)
		unix.Close(fd)
		ok = err == nil
	}
	if !ok {
		log.Warningf("fbtenant: cannot set SO_MARK on sockets (CAP_NET_ADMIN missing?); dns_via_tunnel falls back to unmarked queries")
	}
	c.markSupported = &ok
	return ok
}

// spec parses and caches one upstream address; nil for unparseable ones.
func (c *dnsUpstreamClient) spec(addr string) *UpstreamSpec {
	if v, ok := c.specs.Load(addr); ok {
		return v.(*UpstreamSpec)
	}
	sp, err := ParseUpstream(addr)
	if err != nil {
		log.Warningf("fbtenant: skipping unparseable upstream %q: %v", addr, err)
		return nil
	}
	c.specs.Store(addr, sp)
	return sp
}

func (c *dnsUpstreamClient) randInt(n int) int {
	if c.randFn != nil {
		return c.randFn(n)
	}
	return rand.Intn(n)
}

// Resolve implements upstreamClient.
func (c *dnsUpstreamClient) Resolve(ctx context.Context, servers []string, policy string, qname string, qtype uint16, mark uint32) (*dns.Msg, error) {
	if len(servers) == 0 {
		return nil, fmt.Errorf("no upstream servers configured")
	}
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(qname), qtype)
	q.RecursionDesired = true

	useMark := mark > 0 && c.probeMarkCapability()
	mk := marker{mark: mark, enabled: useMark}

	timeout := c.timeout
	if timeout <= 0 {
		timeout = upstreamTimeout
	}

	specs := make([]*UpstreamSpec, 0, len(servers))
	for _, s := range servers {
		if sp := c.spec(s); sp != nil {
			specs = append(specs, sp)
		}
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no parseable upstream servers in %s", strings.Join(servers, ","))
	}

	now := time.Now()
	var lastErr error
	for _, sp := range orderForQuery(specs, policy, c.health, now, c.randInt) {
		tr := transportForScheme(sp.Scheme)
		if tr == nil {
			c.health.markFail(sp.Addr(), "no transport for scheme "+sp.Scheme, now)
			lastErr = fmt.Errorf("upstream %s: unsupported scheme", sp.Raw)
			continue
		}
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		resp, err := tr.exchange(attemptCtx, sp, q, mk)
		cancel()
		if err != nil {
			c.health.markFail(sp.Addr(), err.Error(), time.Now())
			log.Debugf("fbtenant: upstream %s failed for %s/%s: %v", sp.Addr(), qname, dns.TypeToString[qtype], err)
			lastErr = err
			continue
		}
		c.health.markOk(sp.Addr(), time.Now())
		return resp, nil
	}
	return nil, fmt.Errorf("upstream resolve %s/%s via %s: %w",
		strings.TrimSuffix(qname, "."), dns.TypeToString[qtype],
		strings.Join(servers, ","), lastErr)
}

// minAnswerTTL returns the smallest TTL among A/AAAA records of the answer
// (0 when there is none).
func minAnswerTTL(m *dns.Msg) time.Duration {
	var minTTL time.Duration = -1
	for _, rr := range m.Answer {
		var ttl uint32
		switch rr.(type) {
		case *dns.A, *dns.AAAA:
			ttl = rr.Header().Ttl
		default:
			continue
		}
		d := time.Duration(ttl) * time.Second
		if minTTL < 0 || d < minTTL {
			minTTL = d
		}
	}
	if minTTL < 0 {
		return 0
	}
	return minTTL
}
