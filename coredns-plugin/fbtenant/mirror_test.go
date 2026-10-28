package fbtenant

import (
	"testing"
	"time"
)

func mirrorTestState(refreshPct, maxSetSize int) *State {
	st := testState()
	st.RefreshOnRemainPct = refreshPct
	st.MaxSetSize = maxSetSize
	return st
}

func TestMirrorAddIfAbsentSkipsDuplicatesWithoutNFTCall(t *testing.T) {
	ops := newMockNFT()
	m := newMirrorManager(ops, "fencebreaker", mirrorTestState(0, 0))
	now := time.Now()

	res := m.AddIfAbsent("t1_inc_dns", []string{"1.2.3.4"}, 300*time.Second, now)
	if res.Added != 1 || res.Skipped != 0 || res.WriteErr != nil {
		t.Fatalf("first add: %+v", res)
	}
	if ops.addCount() != 1 {
		t.Fatalf("expected 1 nft add, got %d", ops.addCount())
	}

	// Same IP, still alive: duplicate must NOT trigger any nft command.
	res = m.AddIfAbsent("t1_inc_dns", []string{"1.2.3.4"}, 300*time.Second, now.Add(time.Second))
	if res.Added != 0 || res.Skipped != 1 {
		t.Fatalf("second add: %+v", res)
	}
	if ops.addCount() != 1 {
		t.Fatalf("duplicate caused an nft add call: %d", ops.addCount())
	}
}

func TestMirrorExpiredEntryIsAddedAgain(t *testing.T) {
	ops := newMockNFT()
	m := newMirrorManager(ops, "fencebreaker", mirrorTestState(0, 0))
	now := time.Now()

	if res := m.AddIfAbsent("t1_inc_dns", []string{"1.2.3.4"}, 300*time.Second, now); res.Added != 1 {
		t.Fatalf("first add: %+v", res)
	}
	// Well past the deadline the entry is re-added.
	res := m.AddIfAbsent("t1_inc_dns", []string{"1.2.3.4"}, 300*time.Second, now.Add(301*time.Second))
	if res.Added != 1 || res.Skipped != 0 {
		t.Fatalf("expired re-add: %+v", res)
	}
	if ops.addCount() != 2 {
		t.Fatalf("expected 2 nft adds, got %d", ops.addCount())
	}
}

func TestMirrorRefreshOnRemainPct(t *testing.T) {
	tests := []struct {
		name      string
		pct       int
		elapsed   time.Duration // since first add; timeout is 300s
		wantAdded int
		wantSkip  int
	}{
		{"pct0 never refreshes", 0, 240 * time.Second, 0, 1},
		{"pct50 remaining below threshold refreshes", 50, 240 * time.Second, 1, 0}, // remaining 60s < 150s
		{"pct50 remaining above threshold skips", 50, 60 * time.Second, 0, 1},      // remaining 240s > 150s
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ops := newMockNFT()
			m := newMirrorManager(ops, "fencebreaker", mirrorTestState(tc.pct, 0))
			now := time.Now()
			if res := m.AddIfAbsent("t1_inc_dns", []string{"1.2.3.4"}, 300*time.Second, now); res.Added != 1 {
				t.Fatalf("first add: %+v", res)
			}
			res := m.AddIfAbsent("t1_inc_dns", []string{"1.2.3.4"}, 300*time.Second, now.Add(tc.elapsed))
			if res.Added != tc.wantAdded || res.Skipped != tc.wantSkip {
				t.Fatalf("refresh add: %+v, want added=%d skip=%d", res, tc.wantAdded, tc.wantSkip)
			}
			wantCalls := 1 + tc.wantAdded
			if ops.addCount() != wantCalls {
				t.Fatalf("nft add calls = %d, want %d", ops.addCount(), wantCalls)
			}
		})
	}
}

func TestMirrorBatchOneCallPerResponse(t *testing.T) {
	ops := newMockNFT()
	m := newMirrorManager(ops, "fencebreaker", mirrorTestState(0, 0))
	now := time.Now()
	res := m.AddIfAbsent("t1_inc_dns", []string{"1.2.3.4", "5.6.7.8", "9.9.9.9"}, 300*time.Second, now)
	if res.Added != 3 {
		t.Fatalf("batch add: %+v", res)
	}
	// A whole batch is one AddElements invocation (one transaction).
	if ops.addCount() != 1 {
		t.Fatalf("expected single batched nft call, got %d", ops.addCount())
	}
	if len(ops.adds[0].elems) != 3 {
		t.Fatalf("batch elements: %v", ops.adds[0].elems)
	}
}

func TestMirrorMaxSetSizeEvictsOldest(t *testing.T) {
	ops := newMockNFT()
	m := newMirrorManager(ops, "fencebreaker", mirrorTestState(0, 3))
	now := time.Now()

	// Fill to the limit with staggered deadlines.
	ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
	for i, ip := range ips {
		if res := m.AddIfAbsent("t1_inc_dns", []string{ip}, 300*time.Second, now.Add(time.Duration(i)*10*time.Second)); res.Added != 1 {
			t.Fatalf("fill %s: %+v", ip, res)
		}
	}
	// One more: the oldest (1.1.1.1, earliest deadline) must be evicted.
	res := m.AddIfAbsent("t1_inc_dns", []string{"4.4.4.4"}, 300*time.Second, now.Add(time.Minute))
	if res.Added != 1 || res.Evicted != 1 || res.WriteErr != nil || res.EvictErr != nil {
		t.Fatalf("overflow add: %+v", res)
	}
	if m.SetSize("t1_inc_dns") != 3 {
		t.Fatalf("set size after eviction = %d, want 3", m.SetSize("t1_inc_dns"))
	}
	if ops.delCount() != 1 {
		t.Fatalf("expected one DeleteElements call, got %d", ops.delCount())
	}
	if got := ops.dels[0]; got.set != "t1_inc_dns" || len(got.ips) != 1 || got.ips[0] != "1.1.1.1" {
		t.Fatalf("evicted wrong element: %+v", got)
	}
	// Mirror agrees: 1.1.1.1 gone, 4.4.4.4 present.
	if _, ok := m.sets["t1_inc_dns"].entries["1.1.1.1"]; ok {
		t.Fatal("evicted ip still in mirror")
	}
	if _, ok := m.sets["t1_inc_dns"].entries["4.4.4.4"]; !ok {
		t.Fatal("new ip missing from mirror")
	}
}

func TestMirrorResyncPicksUpAndDrops(t *testing.T) {
	ops := newMockNFT()
	m := newMirrorManager(ops, "fencebreaker", mirrorTestState(0, 0))
	now := time.Now()

	// Learn something locally first.
	if res := m.AddIfAbsent("t1_inc_dns", []string{"1.2.3.4"}, 300*time.Second, now); res.Added != 1 {
		t.Fatalf("first add: %+v", res)
	}
	// nftables reality: our entry expired and vanished, an unknown one exists.
	ops.mu.Lock()
	ops.sets["t1_inc_dns"] = map[string]setElem{
		"5.5.5.5": {Remaining: 100 * time.Second, Timeout: 200 * time.Second},
	}
	ops.mu.Unlock()

	if err := m.Resync(now.Add(time.Second)); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if m.SetSize("t1_inc_dns") != 1 {
		t.Fatalf("set size after resync = %d, want 1", m.SetSize("t1_inc_dns"))
	}
	e, ok := m.sets["t1_inc_dns"].entries["5.5.5.5"]
	if !ok {
		t.Fatal("unknown nft element not picked up")
	}
	// deadline = resync time + remaining
	if want := now.Add(time.Second).Add(100 * time.Second); !e.deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v", e.deadline, want)
	}
	if _, ok := m.sets["t1_inc_dns"].entries["1.2.3.4"]; ok {
		t.Fatal("expired entry not dropped from mirror")
	}
	// After resync the previously learned IP is a duplicate from nft's point
	// of view only while alive; adding it again must go through.
	if res := m.AddIfAbsent("t1_inc_dns", []string{"1.2.3.4"}, 300*time.Second, now.Add(2*time.Second)); res.Added != 1 {
		t.Fatalf("add after resync: %+v", res)
	}
}

func TestMirrorResyncErrorKeepsMirrorAndCounts(t *testing.T) {
	ops := newMockNFT()
	ops.listErr["t1_exc_dns"] = &staticError{"nft boom"}
	m := newMirrorManager(ops, "fencebreaker", mirrorTestState(0, 0))

	if err := m.Resync(time.Now()); err == nil {
		t.Fatal("expected resync error")
	}
	if _, count := m.ResyncStats(); count != 0 {
		t.Fatalf("resync count on failure = %d, want 0", count)
	}

	ops.mu.Lock()
	delete(ops.listErr, "t1_exc_dns")
	ops.mu.Unlock()
	if err := m.Resync(time.Now()); err != nil {
		t.Fatalf("resync after fix: %v", err)
	}
	if _, count := m.ResyncStats(); count != 1 {
		t.Fatalf("resync count = %d, want 1", count)
	}
}

func TestMirrorUpdateKnobsDropsRemovedSets(t *testing.T) {
	ops := newMockNFT()
	m := newMirrorManager(ops, "fencebreaker", mirrorTestState(0, 0))
	if m.SetSize("t1_inc_dns") != 0 || m.SetSize("t1_exc_dns") != 0 {
		t.Fatal("expected both tenant sets tracked")
	}
	st := mirrorTestState(0, 0)
	st.Tenants[0].Sets = map[string]string{"inc_dns": "t1_inc_dns"} // exc_dns removed
	m.updateKnobs(st)
	names := m.SetNames()
	if len(names) != 1 || names[0] != "t1_inc_dns" {
		t.Fatalf("set names after knob update: %v", names)
	}
}

type staticError struct{ msg string }

func (e *staticError) Error() string { return e.msg }
