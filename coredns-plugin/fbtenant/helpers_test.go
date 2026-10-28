package fbtenant

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// ---- mock NFTOps -----------------------------------------------------------

type addCall struct {
	table string
	set   string
	elems map[string]time.Duration
}

type delCall struct {
	table string
	set   string
	ips   []string
}

// mockNFT records every NFTOps call and keeps an in-memory set state.
type mockNFT struct {
	mu      sync.Mutex
	sets    map[string]map[string]setElem
	adds    []addCall
	dels    []delCall
	listErr map[string]error
	addErr  map[string]error
}

func newMockNFT() *mockNFT {
	return &mockNFT{sets: map[string]map[string]setElem{}, listErr: map[string]error{}, addErr: map[string]error{}}
}

func (m *mockNFT) ListSet(table, set string) (map[string]setElem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.listErr[set]; err != nil {
		return nil, err
	}
	out := map[string]setElem{}
	for ip, e := range m.sets[set] {
		out[ip] = e
	}
	return out, nil
}

func (m *mockNFT) AddElements(table, set string, elems map[string]time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.addErr[set]; err != nil {
		return err
	}
	cp := map[string]time.Duration{}
	for ip, d := range elems {
		cp[ip] = d
	}
	m.adds = append(m.adds, addCall{table: table, set: set, elems: cp})
	if m.sets[set] == nil {
		m.sets[set] = map[string]setElem{}
	}
	for ip, d := range elems {
		m.sets[set][ip] = setElem{Remaining: d, Timeout: d}
	}
	return nil
}

func (m *mockNFT) DeleteElements(table, set string, ips []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := append([]string(nil), ips...)
	sort.Strings(cp)
	m.dels = append(m.dels, delCall{table: table, set: set, ips: cp})
	for _, ip := range ips {
		delete(m.sets[set], ip)
	}
	return nil
}

func (m *mockNFT) addCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.adds)
}

func (m *mockNFT) delCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.dels)
}

// ---- test DNS servers ------------------------------------------------------

// countedHandler answers every A/AAAA question with fixed IPs and counts
// queries; it records which server instance was hit.
type countedHandler struct {
	mu     sync.Mutex
	count  int
	aIP    net.IP
	aaaaIP net.IP
	ttl    uint32
	// perName overrides aIP/aaaaIP for specific (lowercase, no trailing dot)
	// qnames, letting one server answer different names with different IPs.
	perName map[string]string
	nxFor   map[string]bool // qname -> reply NXDOMAIN
	// forceTC makes UDP-facing servers reply with TC=1 (the caller wires
	// separate UDP and TCP servers on the same port).
	forceTC bool
}

func (h *countedHandler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	h.mu.Lock()
	h.count++
	h.mu.Unlock()
	m := new(dns.Msg)
	m.SetReply(r)
	qname := strings.ToLower(strings.TrimSuffix(r.Question[0].Name, "."))
	if h.nxFor != nil && h.nxFor[qname] {
		m.SetRcode(r, dns.RcodeNameError)
		_ = w.WriteMsg(m)
		return
	}
	if r.Question[0].Qtype == dns.TypeAAAA && h.aaaaIP != nil {
		m.Answer = append(m.Answer, &dns.AAAA{
			Hdr:  dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: h.ttl},
			AAAA: h.aaaaIP,
		})
	} else {
		ip := h.aIP
		if h.perName != nil {
			if s, ok := h.perName[qname]; ok {
				ip = net.ParseIP(s).To4()
			}
		}
		if ip != nil {
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: h.ttl},
				A:   ip,
			})
		}
	}
	if h.forceTC {
		m.Truncated = true
		m.Answer = nil
	}
	_ = w.WriteMsg(m)
}

func (h *countedHandler) hits() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// startUDPServer starts a UDP-only miekg/dns server on a random port.
func startUDPServer(t *testing.T, handler dns.Handler) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("udp listen: %v", err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: handler}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

// startUDPTCPServer binds one port for both UDP and TCP with different
// handlers, emulating a resolver that truncates over UDP.
func startUDPTCPServer(t *testing.T, udpHandler, tcpHandler dns.Handler) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("udp listen: %v", err)
	}
	addr := pc.LocalAddr().String()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}
	udpSrv := &dns.Server{PacketConn: pc, Handler: udpHandler}
	tcpSrv := &dns.Server{Listener: ln, Handler: tcpHandler}
	go func() { _ = udpSrv.ActivateAndServe() }()
	go func() { _ = tcpSrv.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = udpSrv.Shutdown()
		_ = tcpSrv.Shutdown()
	})
	return addr
}

// ---- plugin builder --------------------------------------------------------

// testState returns a base state matching the CONTRACTS §2 example shape.
func testState() *State {
	return &State{
		Version:            1,
		Generation:         3,
		ListenPort:         1053,
		ResyncIntervalSec:  60,
		TTLMinSec:          60,
		TTLMaxSec:          600,
		RefreshOnRemainPct: 0,
		MaxSetSize:         65536,
		DefaultUpstreams:   []string{"127.0.0.1:53"},
		NFTTable:           "fencebreaker",
		Tenants: []Tenant{
			{
				ID:             "tenant1",
				Mark:           4096,
				SrcNets:        []string{"192.168.100.0/24"},
				HostsFull:      []string{"192.168.100.7"},
				Sets:           map[string]string{"inc_dns": "t1_inc_dns", "exc_dns": "t1_exc_dns"},
				LearningMode:   "select",
				IncludeDomains: []string{"example.com"},
				Upstreams:      []string{"127.0.0.1:53"},
				DNSViaTunnel:   true,
				Records: []Record{
					{Domain: "api.corp", IPs: []string{"10.0.0.5"}},
				},
				Forwards: []Forward{
					{Domain: "corp.example", Servers: []string{"forward-placeholder"}},
				},
			},
		},
	}
}

// newTestPlugin builds a plugin around the given state and NFT mock, with
// fast loop intervals. statusFile points into a temp dir.
func newTestPlugin(t *testing.T, st *State, ops NFTOps) *FBTenant {
	t.Helper()
	f := New()
	f.pollInterval = 10 * time.Millisecond
	f.statusInterval = 20 * time.Millisecond
	f.setNFTOpsForTest(ops)
	cfg, err := prepare(st)
	if err != nil {
		t.Fatalf("prepare state: %v", err)
	}
	f.setConfig(cfg)
	f.statusFile = fmt.Sprintf("%s/plugin_status.json", t.TempDir())
	return f
}

// waitFor polls cond until it holds or the timeout elapses.
func waitFor(t *testing.T, timeout, step time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(step)
	}
	t.Fatalf("condition not met within %v: %s", timeout, msg)
}
