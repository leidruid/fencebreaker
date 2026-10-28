package fbtenant

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

// FBTenant is the fbtenant CoreDNS plugin. It resolves per-tenant DNS,
// learns A records from answers and mirrors them into nftables timeout sets
// (CONTRACTS §2–§4).
type FBTenant struct {
	Next plugin.Handler

	stateFile string

	// statusFile is where plugin_status.json is written (default:
	// directory of the state file + /plugin_status.json).
	statusFile string

	// Background loop knobs (defaults: 2s state poll, 5s status write,
	// 15s keeper scan); shortened in tests.
	pollInterval   time.Duration
	statusInterval time.Duration
	keeperInterval time.Duration
	keeperSem      chan struct{}
	keepers        map[string]*tenantKeeper // tenant ID -> registry, survives config reloads
	keeperMu       sync.Mutex
	keeperFile     string // binary registry next to the state file

	client upstreamClient
	nft    NFTOps

	// cfg holds the active config snapshot, replaced on state reload.
	cfg atomic.Pointer[config]

	mirrors *mirrorManager
	mirMu   sync.Mutex // guards mirrors replacement

	resyncMu     sync.Mutex // guards nextResyncAt/backoff
	nextResyncAt time.Time
	backoff      time.Duration

	stopCh chan struct{}
}

var _ plugin.Handler = (*FBTenant)(nil)

// Name implements plugin.Handler.
func (f *FBTenant) Name() string { return "fbtenant" }

// New returns a plugin with production defaults.
func New() *FBTenant {
	return &FBTenant{
		client:         newDNSUpstreamClient(),
		nft:            newNFTOps(),
		pollInterval:   2 * time.Second,
		statusInterval: 5 * time.Second,
		keeperInterval: keeperTick,
		keeperSem:      make(chan struct{}, keeperConcurrency),
		stopCh:         make(chan struct{}),
	}
}

// currentCfg returns the active config snapshot (nil until first load).
func (f *FBTenant) currentCfg() *config { return f.cfg.Load() }

// setConfig swaps in a new config and updates mirror knobs.
func (f *FBTenant) setConfig(cfg *config) {
	f.mirMu.Lock()
	if f.mirrors == nil {
		f.mirrors = newMirrorManager(f.nft, cfg.table, &cfg.state)
	} else {
		f.mirrors.updateKnobs(&cfg.state)
	}
	f.mirMu.Unlock()
	f.cfg.Store(cfg)
}

// setNFTOpsForTest replaces the nftables backend (mirrors must not exist yet).
func (f *FBTenant) setNFTOpsForTest(ops NFTOps) {
	f.nft = ops
	f.mirMu.Lock()
	f.mirrors = nil
	f.mirMu.Unlock()
}

// ServeDNS implements plugin.Handler. It is self-sufficient: for requests it
// serves it never calls next.
func (f *FBTenant) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	if len(r.Question) == 0 {
		return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
	}
	req := request.Request{Req: r, W: w}
	qname := strings.ToLower(strings.TrimSuffix(req.Name(), "."))
	qtype := req.QType()

	cfg := f.currentCfg()
	if cfg == nil {
		// Not configured yet: let the chain handle it.
		return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
	}

	clientIP := net.ParseIP(req.IP())
	tenant := cfg.tenantsByIPClient.lookup(clientIP)

	if tenant == nil {
		// Unknown client: resolve via default_upstreams, no learning,
		// no mark, no cache.
		if len(cfg.defaultUpstreams) == 0 {
			return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
		}
		resp, err := f.client.Resolve(ctx, cfg.defaultUpstreams, PolicySequential, qname, qtype, 0)
		if err != nil {
			log.Warningf("fbtenant: default upstream resolve failed for %s/%s: %v", qname, dns.TypeToString[qtype], err)
			return dns.RcodeServerFailure, w.WriteMsg(servfail(r))
		}
		return f.writeAnswer(w, r, resp), nil
	}

	tenant.stats.queries.Add(1)

	// Exact record override (A only), answered immediately.
	if qtype == dns.TypeA {
		if ips, ok := tenant.records[qname]; ok {
			ttl := clampTTL(0, ttlMin(&cfg.state), ttlMax(&cfg.state))
			m := new(dns.Msg)
			m.SetReply(r)
			for _, ip := range ips {
				m.Answer = append(m.Answer, &dns.A{
					Hdr: dns.RR_Header{
						Name:   dns.Fqdn(qname),
						Rrtype: dns.TypeA,
						Class:  dns.ClassINET,
						Ttl:    uint32(ttl / time.Second),
					},
					A: ip,
				})
			}
			return f.writeAnswer(w, r, m), nil
		}
	}

	// Per-tenant cache.
	key := tenant.raw.ID + "|" + cacheKey(qname, qtype)
	if hit := tenant.cache.get(key, time.Now()); hit != nil {
		tenant.stats.cacheHits.Add(1)
		return f.writeAnswer(w, r, hit), nil
	}

	// Pick upstreams: narrowest forward suffix, else tenant upstreams.
	mark := uint32(0)
	if tenant.raw.DNSViaTunnel {
		mark = tenant.raw.Mark
	}
	servers := tenant.forwards.lookup(qname)
	policy := tenant.upstreamPolicy
	if len(servers) == 0 {
		servers = tenant.upstreams
	}

	resp, err := f.client.Resolve(ctx, servers, policy, qname, qtype, mark)
	if err != nil {
		tenant.stats.upstreamErrors.Add(1)
		tenant.stats.setLastError(err.Error())
		log.Warningf("fbtenant: tenant %s upstream resolve failed for %s/%s: %v", tenant.raw.ID, qname, dns.TypeToString[qtype], err)
		return dns.RcodeServerFailure, w.WriteMsg(servfail(r))
	}

	// Learning: A records of positive answers only; AAAA is resolved and
	// returned but never learned. NXDOMAIN/empty answers are just returned.
	if qtype == dns.TypeA && resp.Rcode == dns.RcodeSuccess && len(resp.Answer) > 0 {
		if kind := tenant.learnTarget(qname); kind != "" {
			f.learn(&cfg.state, tenant, kind, qname, resp, time.Now())
		}
	}

	// Cache positive answers.
	if resp.Rcode == dns.RcodeSuccess && len(resp.Answer) > 0 {
		if ttl := minAnswerTTL(resp); ttl > 0 {
			tenant.cache.put(key, resp, ttl, time.Now())
		}
	}

	return f.writeAnswer(w, r, resp), nil
}

// learn sends the A-record IPs of an answer to the tenant's nftables set
// (kind is SetIncDNS or SetExcDNS) through the mirror; duplicate filtering
// happens inside the mirror. With dns_keepalive the domain also joins the
// tenant's keeper registry, aligned with the live element deadlines.
func (f *FBTenant) learn(state *State, tenant *preparedTenant, kind, qname string, resp *dns.Msg, now time.Time) {
	setName := tenant.setName(kind)
	m := f.getMirrors()
	if setName == "" || m == nil {
		return
	}
	var ips []string
	qttl := time.Duration(0)
	for _, rr := range resp.Answer {
		a, ok := rr.(*dns.A)
		if !ok {
			continue
		}
		ips = append(ips, a.A.String())
		if t := time.Duration(a.Hdr.Ttl) * time.Second; qttl == 0 || t < qttl {
			qttl = t
		}
	}
	if len(ips) == 0 {
		return
	}
	timeout := clampTTL(qttl, ttlMin(state), ttlMax(state))
	res := m.AddIfAbsent(setName, ips, timeout, now)
	if res.Added > 0 {
		tenant.stats.learned.Add(int64(res.Added))
	}
	if res.Skipped > 0 {
		tenant.stats.duplicatesSkipped.Add(int64(res.Skipped))
	}
	if res.EvictErr != nil {
		tenant.stats.setLastError(res.EvictErr.Error())
		log.Warningf("fbtenant: tenant %s %v", tenant.raw.ID, res.EvictErr)
	}
	if res.WriteErr != nil {
		tenant.stats.setLastError(res.WriteErr.Error())
		log.Warningf("fbtenant: tenant %s %v", tenant.raw.ID, res.WriteErr)
		f.scheduleResyncSoon()
	}
	if !tenant.keepalive {
		return
	}
	// Align the keeper deadline with the mirror: duplicate-skipped elements
	// keep their earlier deadline, which is when routing would break.
	deadline, ok := m.MinDeadline(setName, ips, now)
	if !ok {
		deadline = now.Add(timeout)
	}
	f.getKeeper(tenant.raw.ID).register(qname, kind, timeout, deadline)
}

func (f *FBTenant) getMirrors() *mirrorManager {
	f.mirMu.Lock()
	defer f.mirMu.Unlock()
	return f.mirrors
}

// writeAnswer writes msg to w with the request id fixed; returns the rcode.
func (f *FBTenant) writeAnswer(w dns.ResponseWriter, r *dns.Msg, msg *dns.Msg) int {
	msg.Id = r.Id
	msg.Response = true
	rcode := msg.Rcode
	if rcode == 0 {
		rcode = dns.RcodeSuccess
	}
	if err := w.WriteMsg(msg); err != nil {
		log.Warningf("fbtenant: write response: %v", err)
	}
	return rcode
}

func servfail(r *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetRcode(r, dns.RcodeServerFailure)
	return m
}

func ttlMin(st *State) time.Duration {
	s := st.TTLMinSec
	if s <= 0 {
		s = 300
	}
	return time.Duration(s) * time.Second
}

func ttlMax(st *State) time.Duration {
	s := st.TTLMaxSec
	if s < st.TTLMinSec {
		s = st.TTLMinSec
	}
	if s <= 0 {
		s = 86400
	}
	return time.Duration(s) * time.Second
}

// lookupTenant exposes tenant lookup for table-driven tests.
func (f *FBTenant) lookupTenant(ip string) *preparedTenant {
	cfg := f.currentCfg()
	if cfg == nil {
		return nil
	}
	return cfg.tenantsByIPClient.lookup(net.ParseIP(ip))
}

// mirrorSize returns the mirrored size of one set (0 when unknown).
func (f *FBTenant) mirrorSize(setName string) int {
	m := f.getMirrors()
	if m == nil {
		return 0
	}
	return m.SetSize(setName)
}
