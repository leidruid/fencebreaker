package fbtenant

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
)

func query(qname string, qtype uint16, remoteIP string) (*dnstest.Recorder, *dns.Msg) {
	w := &test.ResponseWriter{RemoteIP: remoteIP}
	rec := dnstest.NewRecorder(w)
	r := new(dns.Msg)
	r.SetQuestion(dns.Fqdn(qname), qtype)
	return rec, r
}

func serve(f *FBTenant, rec *dnstest.Recorder, r *dns.Msg) (int, error) {
	return f.ServeDNS(context.Background(), rec, r)
}

func answerIP(m *dns.Msg) string {
	if m == nil || len(m.Answer) == 0 {
		return ""
	}
	switch a := m.Answer[0].(type) {
	case *dns.A:
		return a.A.String()
	case *dns.AAAA:
		return a.AAAA.String()
	}
	return ""
}

// ---- ServeDNS: exact records ----------------------------------------------

func TestServeDNSServesExactRecordWithoutUpstream(t *testing.T) {
	ops := newMockNFT()
	f := newTestPlugin(t, testState(), ops)

	rec, r := query("api.corp", dns.TypeA, "192.168.100.5")
	rcode, err := serve(f, rec, r)
	if err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %d", rcode)
	}
	if got := answerIP(rec.Msg); got != "10.0.0.5" {
		t.Fatalf("answer ip = %q, want 10.0.0.5", got)
	}
	if len(rec.Msg.Answer) != 1 || rec.Msg.Answer[0].Header().Name != "api.corp." {
		t.Fatalf("answer section: %+v", rec.Msg.Answer)
	}
}

// ---- ServeDNS: unknown client goes to default_upstreams, no learning ------

func TestServeDNSUnknownClientUsesDefaultUpstreams(t *testing.T) {
	ops := newMockNFT()
	st := testState()
	h := &countedHandler{aIP: ip4("7.7.7.7"), ttl: 120}
	st.DefaultUpstreams = []string{startUDPServer(t, h)}
	f := newTestPlugin(t, st, ops)

	rec, r := query("www.example.com", dns.TypeA, "10.240.0.1") // no tenant match
	if _, err := serve(f, rec, r); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if got := answerIP(rec.Msg); got != "7.7.7.7" {
		t.Fatalf("answer ip = %q, want 7.7.7.7 from default upstream", got)
	}
	// No learning for unknown clients.
	if ops.addCount() != 0 {
		t.Fatalf("unknown client learned into nft: %d adds", ops.addCount())
	}
	// No per-tenant counters either.
	sts := f.tenantStatuses()
	if sts["tenant1"].Queries != 0 {
		t.Fatalf("tenant queries counted for unknown client: %+v", sts)
	}
}

// ---- ServeDNS: tenant upstream + learning (select) ------------------------

func TestServeDNSTenantLearnsARecords(t *testing.T) {
	ops := newMockNFT()
	st := testState()
	h := &countedHandler{
		ttl: 120,
		perName: map[string]string{
			"www.example.com":   "9.9.9.9",
			"other.example.com": "9.9.9.9", // same IP as www: duplicate candidate
			"bank.ru":           "8.8.8.8", // outside include_domains
		},
	}
	st.Tenants[0].Upstreams = []string{startUDPServer(t, h)}
	f := newTestPlugin(t, st, ops)

	rec, r := query("www.example.com", dns.TypeA, "192.168.100.5") // src_nets match
	if _, err := serve(f, rec, r); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if got := answerIP(rec.Msg); got != "9.9.9.9" {
		t.Fatalf("answer ip = %q", got)
	}
	// Learned into the tenant set with TTL clamped into [60,600] -> 120s.
	if ops.addCount() != 1 {
		t.Fatalf("expected 1 add, got %d", ops.addCount())
	}
	add := ops.adds[0]
	if add.set != "t1_inc_dns" || add.elems["9.9.9.9"] != 120*time.Second {
		t.Fatalf("learned element: %+v", add)
	}
	sts := f.tenantStatuses()
	if sts["tenant1"].Learned != 1 || sts["tenant1"].Queries != 1 {
		t.Fatalf("tenant stats: %+v", sts["tenant1"])
	}

	// Same IP learned again via a different qname that is still in the mirror:
	// duplicate is skipped without an nft call.
	rec2, r2 := query("other.example.com", dns.TypeA, "192.168.100.5")
	if _, err := serve(f, rec2, r2); err != nil {
		t.Fatalf("ServeDNS 2: %v", err)
	}
	if ops.addCount() != 1 {
		t.Fatalf("duplicate add executed: %d calls", ops.addCount())
	}
	sts = f.tenantStatuses()
	if sts["tenant1"].DuplicatesSkipped != 1 {
		t.Fatalf("duplicates_skipped = %d, want 1", sts["tenant1"].DuplicatesSkipped)
	}

	// Domain outside include_domains is not learned at all (its IP is new,
	// so a leak would show up as a second add).
	rec3, r3 := query("bank.ru", dns.TypeA, "192.168.100.5")
	if _, err := serve(f, rec3, r3); err != nil {
		t.Fatalf("ServeDNS 3: %v", err)
	}
	if ops.addCount() != 1 {
		t.Fatalf("select-mode learning added a non-included domain")
	}
}

// ---- ServeDNS: cache --------------------------------------------------------

func TestServeDNSCacheHitsPerTenant(t *testing.T) {
	ops := newMockNFT()
	st := testState()
	h := &countedHandler{aIP: ip4("9.9.9.9"), ttl: 120}
	st.Tenants[0].Upstreams = []string{startUDPServer(t, h)}
	f := newTestPlugin(t, st, ops)

	for i := 0; i < 3; i++ {
		rec, r := query("cached.example.com", dns.TypeA, "192.168.100.5")
		if _, err := serve(f, rec, r); err != nil {
			t.Fatalf("ServeDNS #%d: %v", i, err)
		}
		if got := answerIP(rec.Msg); got != "9.9.9.9" {
			t.Fatalf("query #%d answer: %q", i, got)
		}
	}
	// One upstream hit, two cache hits.
	if h.hits() != 1 {
		t.Fatalf("upstream hits = %d, want 1", h.hits())
	}
	sts := f.tenantStatuses()
	if sts["tenant1"].CacheHits != 2 {
		t.Fatalf("cache_hits = %d, want 2", sts["tenant1"].CacheHits)
	}
}

// Negative answers are not cached and not learned.
func TestServeDNSNXDOMAINPassthrough(t *testing.T) {
	ops := newMockNFT()
	st := testState()
	h := &countedHandler{ttl: 120, nxFor: map[string]bool{"missing.example.com": true}}
	st.Tenants[0].Upstreams = []string{startUDPServer(t, h)}
	f := newTestPlugin(t, st, ops)

	rec, r := query("missing.example.com", dns.TypeA, "192.168.100.5")
	if _, err := serve(f, rec, r); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if rec.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", rec.Rcode)
	}
	if ops.addCount() != 0 {
		t.Fatal("NXDOMAIN was learned")
	}
	if f.tenantStatuses()["tenant1"].CacheHits != 0 {
		t.Fatal("NXDOMAIN cached")
	}
}

// AAAA is resolved and returned but never learned.
func TestServeDNSAAAAResolvedNotLearned(t *testing.T) {
	ops := newMockNFT()
	st := testState()
	h := &countedHandler{aaaaIP: net.ParseIP("2001:db8::1").To16(), ttl: 120}
	st.Tenants[0].Upstreams = []string{startUDPServer(t, h)}
	f := newTestPlugin(t, st, ops)

	rec, r := query("v6.example.com", dns.TypeAAAA, "192.168.100.5")
	if _, err := serve(f, rec, r); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if got := answerIP(rec.Msg); got != "2001:db8::1" {
		t.Fatalf("answer = %q, want 2001:db8::1", got)
	}
	if ops.addCount() != 0 {
		t.Fatal("AAAA was learned")
	}
}

// ---- ServeDNS: forwards (narrowest suffix wins) ----------------------------

func TestServeDNSForwardTrieRouting(t *testing.T) {
	ops := newMockNFT()
	st := testState()
	tenantUp := &countedHandler{aIP: ip4("1.1.1.1"), ttl: 60}
	wideFwd := &countedHandler{aIP: ip4("2.2.2.2"), ttl: 60}
	narrowFwd := &countedHandler{aIP: ip4("3.3.3.3"), ttl: 60}
	st.Tenants[0].Upstreams = []string{startUDPServer(t, tenantUp)}
	st.Tenants[0].Forwards = []Forward{
		{Domain: "corp.example", Servers: []string{startUDPServer(t, wideFwd)}},
		{Domain: "api.corp.example", Servers: []string{startUDPServer(t, narrowFwd)}},
	}
	f := newTestPlugin(t, st, ops)

	cases := []struct {
		qname  string
		wantIP string
	}{
		{"host.corp.example", "2.2.2.2"},
		{"x.api.corp.example", "3.3.3.3"},
		{"plain.example.com", "1.1.1.1"}, // tenant upstreams fallback
	}
	for _, tc := range cases {
		rec, r := query(tc.qname, dns.TypeA, "192.168.100.5")
		if _, err := serve(f, rec, r); err != nil {
			t.Fatalf("ServeDNS(%s): %v", tc.qname, err)
		}
		if got := answerIP(rec.Msg); got != tc.wantIP {
			t.Errorf("forward(%s) -> %q, want %q", tc.qname, got, tc.wantIP)
		}
	}
	if tenantUp.hits() != 1 {
		t.Errorf("tenant upstream hits = %d, want 1", tenantUp.hits())
	}
	if wideFwd.hits() != 1 || narrowFwd.hits() != 1 {
		t.Errorf("forward hits: wide=%d narrow=%d, want 1 and 1", wideFwd.hits(), narrowFwd.hits())
	}
}

// ---- ServeDNS: upstream failure -> SERVFAIL --------------------------------

func TestServeDNSUpstreamErrorServfail(t *testing.T) {
	ops := newMockNFT()
	st := testState()
	st.Tenants[0].Upstreams = []string{"127.0.0.1:1"}
	f := newTestPlugin(t, st, ops)

	rec, r := query("fail.example.com", dns.TypeA, "192.168.100.5")
	rcode, err := serve(f, rec, r)
	if err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if rcode != dns.RcodeServerFailure || rec.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode=%d rec=%d, want SERVFAIL", rcode, rec.Rcode)
	}
	sts := f.tenantStatuses()
	if sts["tenant1"].UpstreamErrors != 1 || sts["tenant1"].LastError == "" {
		t.Fatalf("tenant stats after failure: %+v", sts["tenant1"])
	}
}

// ---- plugin_status.json ------------------------------------------------------

func TestWriteStatusAtomic(t *testing.T) {
	ops := newMockNFT()
	st := testState()
	h := &countedHandler{aIP: ip4("9.9.9.9"), ttl: 120}
	st.Tenants[0].Upstreams = []string{startUDPServer(t, h)}
	f := newTestPlugin(t, st, ops)

	rec, r := query("www.example.com", dns.TypeA, "192.168.100.5")
	if _, err := serve(f, rec, r); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if err := f.resyncAll("test"); err != nil {
		t.Fatalf("resyncAll: %v", err)
	}

	data, err := os.ReadFile(f.statusFile)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	var doc statusDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse status: %v", err)
	}
	if doc.Generation != 3 {
		t.Errorf("generation = %d, want 3", doc.Generation)
	}
	if doc.Resyncs != 1 || doc.LastResync == "" {
		t.Errorf("resyncs = %d, last_resync = %q", doc.Resyncs, doc.LastResync)
	}
	ts, ok := doc.Tenants["tenant1"]
	if !ok {
		t.Fatalf("tenant1 missing: %+v", doc.Tenants)
	}
	if ts.Queries != 1 || ts.Learned != 1 || ts.DuplicatesSkipped != 0 {
		t.Errorf("tenant1 counters: %+v", ts)
	}
	if ts.SetSizes["inc_dns"] != 1 {
		t.Errorf("set_sizes: %+v", ts.SetSizes)
	}
	// No leftover temp files next to the status file.
	entries, _ := os.ReadDir(filepath.Dir(f.statusFile))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".plugin_status") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// ---- watcher: generation change forces resync, unknown entries picked up ---

func TestWatcherGenerationChangeTriggersResync(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "dns_state.json")
	ops := newMockNFT()
	st := testState()
	writeState(t, statePath, st)

	f := New()
	f.stateFile = statePath
	f.statusFile = filepath.Join(dir, "plugin_status.json")
	f.pollInterval = 10 * time.Millisecond
	f.statusInterval = 20 * time.Millisecond
	f.setNFTOpsForTest(ops)
	if err := f.reloadState(nil); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.startWatcher(ctx)
	defer f.Stop()

	m := f.getMirrors()
	if m == nil {
		t.Fatal("mirrors not created")
	}
	waitFor(t, 2*time.Second, 10*time.Millisecond, "initial resync", func() bool {
		_, c := m.ResyncStats()
		return c >= 1
	})
	_, afterInitial := m.ResyncStats()

	// Daemon replaced the sets (generation bumped) and seeded an unknown IP.
	st.Generation = 4
	ops.mu.Lock()
	ops.sets["t1_inc_dns"] = map[string]setElem{
		"5.5.5.5": {Remaining: 100 * time.Second, Timeout: 200 * time.Second},
	}
	ops.mu.Unlock()
	writeState(t, statePath, st)

	waitFor(t, 3*time.Second, 10*time.Millisecond, "resync after generation change", func() bool {
		_, c := m.ResyncStats()
		return c > afterInitial
	})
	if m.SetSize("t1_inc_dns") != 1 {
		t.Fatalf("mirror size after generation resync = %d, want 1 (unknown entry picked up)", m.SetSize("t1_inc_dns"))
	}
	if f.currentCfg().generation != 4 {
		t.Fatalf("generation = %d, want 4", f.currentCfg().generation)
	}
}

// ---- watcher: periodic resync by resync_interval_sec -----------------------

func TestWatcherPeriodicResync(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "dns_state.json")
	ops := newMockNFT()
	st := testState()
	st.ResyncIntervalSec = 1 // short timer
	writeState(t, statePath, st)

	f := New()
	f.stateFile = statePath
	f.statusFile = filepath.Join(dir, "plugin_status.json")
	f.pollInterval = 20 * time.Millisecond
	f.statusInterval = 50 * time.Millisecond
	f.setNFTOpsForTest(ops)
	if err := f.reloadState(nil); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.startWatcher(ctx)
	defer f.Stop()

	m := f.getMirrors()
	_, count0 := m.ResyncStats()
	waitFor(t, 4*time.Second, 20*time.Millisecond, "periodic resyncs", func() bool {
		_, c := m.ResyncStats()
		return c >= count0+3
	})
}

// ---- setup: Corefile parsing -------------------------------------------------

func TestSetupCorefileParsing(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "dns_state.json")

	// Valid block.
	c := caddy.NewTestController("dns", "fbtenant {\n\tstate "+statePath+"\n}\n")
	if err := setup(c); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Missing state property.
	c = caddy.NewTestController("dns", "fbtenant {\n\tnonsense 1\n}\n")
	if err := setup(c); err == nil {
		t.Fatal("expected error for unknown property")
	}
	// Block without state.
	c = caddy.NewTestController("dns", "fbtenant {\n}\n")
	if err := setup(c); err == nil {
		t.Fatal("expected error for missing state")
	}
	// No block at all.
	c = caddy.NewTestController("dns", "")
	if err := setup(c); err == nil {
		t.Fatal("expected error for empty block")
	}
}

func writeState(t *testing.T, path string, st *State) {
	t.Helper()
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
