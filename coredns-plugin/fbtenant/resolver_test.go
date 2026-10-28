package fbtenant

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func ip4(s string) net.IP {
	ip := net.ParseIP(s)
	if ip == nil {
		panic("bad test ip " + s)
	}
	return ip.To4()
}

func TestResolveUDP(t *testing.T) {
	h := &countedHandler{aIP: ip4("9.9.9.9"), ttl: 120}
	addr := startUDPServer(t, h)

	c := newDNSUpstreamClient()
	resp, err := c.Resolve(context.Background(), []string{addr}, PolicySequential, "www.example.com", dns.TypeA, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("unexpected answer: %+v", resp)
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok || a.A.String() != "9.9.9.9" {
		t.Fatalf("unexpected A record: %+v", resp.Answer[0])
	}
	if got := minAnswerTTL(resp); got != 120*time.Second {
		t.Fatalf("minAnswerTTL = %v, want 120s", got)
	}
}

func TestResolveTCPFallbackOnTruncation(t *testing.T) {
	udpHandler := &countedHandler{forceTC: true} // truncated, no answers
	tcpHandler := &countedHandler{aIP: ip4("9.9.9.9"), ttl: 120}
	addr := startUDPTCPServer(t, udpHandler, tcpHandler)

	c := newDNSUpstreamClient()
	resp, err := c.Resolve(context.Background(), []string{addr}, PolicySequential, "www.example.com", dns.TypeA, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resp.Truncated {
		t.Fatal("final answer still truncated")
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("expected TCP answer, got %+v", resp)
	}
	if tcpHandler.hits() == 0 {
		t.Fatal("TCP server was never queried: fallback did not happen")
	}
	if udpHandler.hits() == 0 {
		t.Fatal("UDP server was never queried")
	}
}

func TestResolveRetriesAcrossServers(t *testing.T) {
	h := &countedHandler{aIP: ip4("8.8.4.4"), ttl: 60}
	live := startUDPServer(t, h)
	dead := "127.0.0.1:1" // nothing listens here -> immediate refusal

	c := newDNSUpstreamClient()
	resp, err := c.Resolve(context.Background(), []string{dead, live}, PolicySequential, "retry.example.com", dns.TypeA, 0)
	if err != nil {
		t.Fatalf("resolve with retry: %v", err)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("unexpected answer: %+v", resp)
	}
	if h.hits() == 0 {
		t.Fatal("second upstream never queried")
	}
}

func TestResolveAllServersDown(t *testing.T) {
	c := newDNSUpstreamClient()
	if _, err := c.Resolve(context.Background(), []string{"127.0.0.1:1"}, PolicySequential, "x.example.com", dns.TypeA, 0); err == nil {
		t.Fatal("expected error when all upstreams fail")
	}
	if _, err := c.Resolve(context.Background(), nil, PolicySequential, "x.example.com", dns.TypeA, 0); err == nil {
		t.Fatal("expected error with no servers")
	}
}

// TestMarkCapabilityProbe just ensures the probe is safe to call and cached
// (the actual marking needs CAP_NET_ADMIN and is exercised on the router).
func TestMarkCapabilityProbe(t *testing.T) {
	c := newDNSUpstreamClient()
	_ = c.probeMarkCapability()
	first := *c.markSupported
	_ = c.probeMarkCapability()
	if first != *c.markSupported {
		t.Fatal("probe result not cached")
	}
}
