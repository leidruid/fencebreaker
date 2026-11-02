package fbtenant

import (
	"net"
	"sort"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// fakeCT records flushed IPs.
type fakeCT struct {
	calls [][]string
	done  chan struct{}
}

func (f *fakeCT) FlushDst(ips []string) {
	f.calls = append(f.calls, ips)
	close(f.done)
}

func newLearnTestPlugin(t *testing.T) (*FBTenant, *fakeCT) {
	t.Helper()
	f := New()
	f.nft = newMockNFT()
	ft := &fakeCT{done: make(chan struct{})}
	f.ct = ft
	f.setConfig(&config{state: *testState(), table: "fencebreaker"})
	return f, ft
}

func aAnswer(name string, ttl uint32, ips ...string) *dns.Msg {
	m := &dns.Msg{Answer: []dns.RR{}}
	for _, ip := range ips {
		a := new(dns.A)
		a.Hdr = dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}
		a.A = net.ParseIP(ip)
		m.Answer = append(m.Answer, a)
	}
	return m
}

// TestLearnFlushesConntrackForFreshIPs: a first answer flushes all its IPs;
// the same answer again (live elements) flushes nothing.
func TestLearnFlushesConntrackForFreshIPs(t *testing.T) {
	f, ft := newLearnTestPlugin(t)
	st := &f.currentCfg().state
	tenant, err := prepareTenant(st.Tenants[0])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	f.learn(st, tenant, SetIncDNS, "www.example.com", aAnswer("www.example.com.", 120, "1.2.3.4", "5.6.7.8"), now)
	select {
	case <-ft.done:
	case <-time.After(2 * time.Second):
		t.Fatal("flusher not called for fresh IPs")
	}
	got := append([]string(nil), ft.calls[0]...)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "1.2.3.4" || got[1] != "5.6.7.8" {
		t.Fatalf("flushed = %v, want [1.2.3.4 5.6.7.8]", got)
	}

	ft2 := &fakeCT{done: make(chan struct{})}
	f.ct = ft2
	f.learn(st, tenant, SetIncDNS, "www.example.com", aAnswer("www.example.com.", 120, "1.2.3.4", "5.6.7.8"), now.Add(time.Second))
	select {
	case <-ft2.done:
		t.Fatal("flusher called for duplicate elements")
	case <-time.After(300 * time.Millisecond):
	}
}
