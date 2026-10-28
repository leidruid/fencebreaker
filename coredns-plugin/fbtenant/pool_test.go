package fbtenant

import (
	"testing"
	"time"
)

func mustSpec(t *testing.T, raw string) *UpstreamSpec {
	t.Helper()
	sp, err := ParseUpstream(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return sp
}

func TestHealthPenaltyDoubling(t *testing.T) {
	h := newUpstreamHealth()
	now := time.Now()
	key := "94.140.14.140:53"

	h.markFail(key, "boom", now)
	e := h.get(key)
	if e.fails != 1 || !e.until.Equal(now.Add(10*time.Second)) {
		t.Fatalf("after first failure: %+v", e)
	}

	h.markFail(key, "boom", now)
	if got := h.get(key); got.fails != 2 || !got.until.Equal(now.Add(20*time.Second)) {
		t.Fatalf("after second failure: %+v", got)
	}
	h.markFail(key, "boom", now)
	if got := h.get(key); got.until != now.Add(40*time.Second) {
		t.Fatalf("after third failure: %+v", got)
	}
	if h.healthy(key, now.Add(39*time.Second)) {
		t.Fatal("still penalized at 39s")
	}
	if !h.healthy(key, now.Add(41*time.Second)) {
		t.Fatal("healthy again at 41s")
	}

	h.markOk(key, now)
	if got := h.get(key); got.fails != 0 || !got.until.IsZero() || got.ok != 1 {
		t.Fatalf("after success: %+v", got)
	}
	if !h.healthy(key, now) {
		t.Fatal("healthy after success")
	}
}

func TestOrderSequentialKeepsConfiguredOrder(t *testing.T) {
	h := newUpstreamHealth()
	now := time.Now()
	a := mustSpec(t, "1.1.1.1")
	b := mustSpec(t, "8.8.8.8")

	h.markFail(b.Addr(), "down", now)

	order := orderForQuery([]*UpstreamSpec{a, b}, PolicySequential, h, now, nil)
	if order[0] != a || order[1] != b {
		t.Fatalf("sequential order: %s, %s", order[0].Addr(), order[1].Addr())
	}
}

func TestOrderSkipsPenalizedFirst(t *testing.T) {
	h := newUpstreamHealth()
	now := time.Now()
	a := mustSpec(t, "1.1.1.1")
	b := mustSpec(t, "8.8.8.8")

	h.markFail(a.Addr(), "down", now)
	// a's penalty expires sooner than b's
	h.markFail(b.Addr(), "down", now.Add(-10*time.Second))
	h.markFail(b.Addr(), "down", now)

	order := orderForQuery([]*UpstreamSpec{a, b}, PolicySequential, h, now, nil)
	if order[0] != a || order[1] != b {
		t.Fatalf("all-penalized order should favor soonest recovery (a at +10s, b at +20s): got %s first", order[0].Addr())
	}
}

func TestOrderRandomUsesRandFn(t *testing.T) {
	h := newUpstreamHealth()
	now := time.Now()
	specs := []*UpstreamSpec{
		mustSpec(t, "1.1.1.1"),
		mustSpec(t, "8.8.8.8"),
		mustSpec(t, "9.9.9.9"),
	}
	// deterministic shuffle: randFn always picks index 0
	// swaps: (2<->0) then (1<->0) over [1.1.1.1, 8.8.8.8, 9.9.9.9]
	//        -> [9.9.9.9, 8.8.8.8, 1.1.1.1] -> [8.8.8.8, 9.9.9.9, 1.1.1.1]
	calls := 0
	randFn := func(n int) int { calls++; return 0 }
	order := orderForQuery(specs, PolicyRandom, h, now, randFn)
	if calls == 0 {
		t.Fatal("randFn never called")
	}
	want := []*UpstreamSpec{specs[1], specs[2], specs[0]}
	for i := range order {
		if order[i] != want[i] {
			t.Fatalf("shuffle result: %v, want %v", addrs(order), addrs(want))
		}
	}
	// all three still present exactly once
	seen := map[string]bool{}
	for _, sp := range order {
		if seen[sp.Addr()] {
			t.Fatalf("duplicate %s", sp.Addr())
		}
		seen[sp.Addr()] = true
	}
}

func TestHealthSnapshot(t *testing.T) {
	h := newUpstreamHealth()
	now := time.Now()
	h.markFail("1.1.1.1:53", "timeout", now)
	h.markOk("8.8.8.8:53", now)

	snap := h.snapshot([]string{"1.1.1.1:53", "8.8.8.8:53"}, now)
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d", len(snap))
	}
	if snap[0].State != "penalty" || snap[0].Fails != 1 || snap[0].LastError != "timeout" {
		t.Fatalf("snap[0] = %+v", snap[0])
	}
	if snap[1].State != "ok" || snap[1].Success != 1 {
		t.Fatalf("snap[1] = %+v", snap[1])
	}
}

func addrs(specs []*UpstreamSpec) []string {
	var out []string
	for _, sp := range specs {
		out = append(out, sp.Addr())
	}
	return out
}
