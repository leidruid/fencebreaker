package fbtenant

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// ---- refresh window ---------------------------------------------------------

func TestKeeperRefreshWindow(t *testing.T) {
	cases := []struct {
		timeout time.Duration
		window  time.Duration
	}{
		{300 * time.Second, 75 * time.Second},   // timeout/4
		{1000 * time.Second, 250 * time.Second}, // timeout/4
		{40 * time.Second, 60 * time.Second},    // floor
		{240 * time.Second, 60 * time.Second},   // timeout/4 == floor
	}
	for _, tc := range cases {
		r := keepRecord{timeout: tc.timeout, deadline: time.Now().Add(tc.timeout)}
		if got := r.deadline.Sub(refreshAt(r)); got != tc.window {
			t.Errorf("window(timeout=%v) = %v, want %v", tc.timeout, got, tc.window)
		}
	}
}

// ---- registry lifecycle -----------------------------------------------------

func TestKeeperDueAndLifecycle(t *testing.T) {
	k := newTenantKeeper()
	now := time.Now()
	k.register("a.example", SetIncDNS, 600*time.Second, now.Add(600*time.Second))
	// window = 150s: due once less than 150s remains.
	if d := k.due(now.Add(449 * time.Second)); len(d) != 0 {
		t.Fatalf("record due too early: %+v", d)
	}
	if d := k.due(now.Add(451 * time.Second)); len(d) != 1 || d[0].name != "a.example" {
		t.Fatalf("record not due inside window: %+v", d)
	}

	// A failed resolve keeps the record; the third one drops it.
	if k.failed("a.example") {
		t.Fatal("dropped after 1 failure")
	}
	if k.failed("a.example") {
		t.Fatal("dropped after 2 failures")
	}
	if !k.failed("a.example") {
		t.Fatal("not dropped after 3 failures")
	}
	k.remove("a.example") // the caller drops the record once failed reports true
	if k.len() != 0 {
		t.Fatalf("registry not cleared: %d records", k.len())
	}

	// refreshed resets failures and re-arms the deadline.
	k.register("b.example", SetExcDNS, 600*time.Second, now.Add(50*time.Second))
	k.failed("b.example")
	k.refreshed("b.example", 600*time.Second, now)
	if d := k.due(now); len(d) != 0 {
		t.Fatalf("record due right after refresh: %+v", d)
	}
	domains, refreshes, errors, last := k.stats()
	if domains != 1 || refreshes != 1 || errors != 4 || last.IsZero() {
		t.Fatalf("stats = %d/%d/%d/%v", domains, refreshes, errors, last)
	}
}

// ---- binary codec -----------------------------------------------------------

func TestKeeperCodecRoundtrip(t *testing.T) {
	recs := []keeperFileRecord{
		{Tenant: "tenant1", Name: "www.example.com", Kind: SetIncDNS,
			Timeout: 300 * time.Second, Deadline: time.Unix(1730000000, 0).UTC()},
		{Tenant: "tenant2", Name: "bank.ru", Kind: SetExcDNS,
			Timeout: 86400 * time.Second, Deadline: time.Unix(1730086400, 0).UTC()},
	}
	var buf bytes.Buffer
	if err := encodeKeeperFile(&buf, recs); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if buf.Len() >= 120 {
		t.Fatalf("two records encoded into %d bytes; binary format not compact", buf.Len())
	}
	got, err := decodeKeeperFile(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("decoded %d records, want 2", len(got))
	}
	for i, r := range got {
		want := recs[i]
		if r.Tenant != want.Tenant || r.Name != want.Name || r.Kind != want.Kind ||
			r.Timeout != want.Timeout || !r.Deadline.Equal(want.Deadline) {
			t.Fatalf("record %d = %+v, want %+v", i, r, want)
		}
	}
}

func TestKeeperCodecRejectsCorrupt(t *testing.T) {
	valid := func(t *testing.T) []byte {
		t.Helper()
		var buf bytes.Buffer
		err := encodeKeeperFile(&buf, []keeperFileRecord{
			{Tenant: "t1", Name: "a.example", Kind: SetIncDNS,
				Timeout: 300 * time.Second, Deadline: time.Unix(1730000000, 0).UTC()},
		})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return buf.Bytes()
	}
	cases := map[string][]byte{
		"short":        valid(t)[:8],
		"bad magic":    append([]byte("XXXX"), valid(t)[4:]...),
		"truncated":    valid(t)[:len(valid(t))-3],
		"trailing":     append(append([]byte(nil), valid(t)...), 0),
		"empty record": valid(t)[:12], // count says 1, no record follows
	}
	for name, data := range cases {
		if _, err := decodeKeeperFile(bytes.NewReader(data)); err == nil {
			t.Errorf("%s: decode accepted corrupt data", name)
		}
	}

	// Bad kind byte inside an otherwise valid record.
	manual := make([]byte, 0, 32)
	manual = append(manual, keeperFileMagic...)
	manual = append(manual, keeperFileVersion, 0)
	manual = binary.LittleEndian.AppendUint32(manual, 1)
	manual = binary.LittleEndian.AppendUint16(manual, 7)
	manual = append(manual, "a.example"[:7]...)
	manual = append(manual, 2, 't', '1')
	manual = append(manual, 9) // no such kind
	manual = binary.LittleEndian.AppendUint32(manual, 300)
	manual = binary.LittleEndian.AppendUint64(manual, 1730000000)
	if _, err := decodeKeeperFile(bytes.NewReader(manual)); err == nil {
		t.Error("bad kind byte accepted")
	}

	// Encode-side guards.
	var buf bytes.Buffer
	if err := encodeKeeperFile(&buf, []keeperFileRecord{{Tenant: strings.Repeat("x", 100),
		Name: "a.example", Kind: SetIncDNS, Timeout: time.Second,
		Deadline: time.Unix(1730000000, 0).UTC()}}); err == nil {
		t.Error("oversized tenant id accepted")
	}
	buf.Reset()
	if err := encodeKeeperFile(&buf, []keeperFileRecord{{Tenant: "t1",
		Name: "a.example", Kind: SetIncDNS, Timeout: 500 * time.Millisecond,
		Deadline: time.Unix(1730000000, 0).UTC()}}); err == nil {
		t.Error("sub-second timeout accepted")
	}
}

func TestKeeperRestoreDropsStale(t *testing.T) {
	k := newTenantKeeper()
	now := time.Now()
	k.restore(keeperFileRecord{Tenant: "t", Name: "old.example", Kind: SetIncDNS,
		Timeout: 300 * time.Second, Deadline: now.Add(-25 * time.Hour)}, now)
	k.restore(keeperFileRecord{Tenant: "t", Name: "live.example", Kind: SetIncDNS,
		Timeout: 300 * time.Second, Deadline: now.Add(-1 * time.Hour)}, now)
	if k.len() != 1 {
		t.Fatalf("stale filter kept %d records, want 1", k.len())
	}
	// The live record's window has long opened: due immediately.
	if d := k.due(now); len(d) != 1 || d[0].name != "live.example" {
		t.Fatalf("live record not due after restore: %+v", d)
	}
}

// ---- mirror.Refresh / MinDeadline -------------------------------------------

func TestMirrorRefreshExtendsDeadline(t *testing.T) {
	ops := newMockNFT()
	m := newMirrorManager(ops, "fencebreaker", &State{})
	now := time.Now()

	if res := m.AddIfAbsent("t1_inc_dns", []string{"9.9.9.9"}, 120*time.Second, now); res.Added != 1 {
		t.Fatalf("first add: %+v", res)
	}
	// Duplicate with refresh_on_remain_pct=0: skipped, nft untouched.
	if res := m.AddIfAbsent("t1_inc_dns", []string{"9.9.9.9"}, 120*time.Second, now.Add(10*time.Second)); res.Skipped != 1 || ops.addCount() != 1 {
		t.Fatalf("duplicate not skipped: %+v", res)
	}
	// Refresh is unconditional.
	if err := m.Refresh("t1_inc_dns", []string{"9.9.9.9"}, 300*time.Second, now.Add(20*time.Second)); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if ops.addCount() != 2 {
		t.Fatalf("refresh did not reach nft: %d adds", ops.addCount())
	}
	// New element lifetime comes from the refreshed timeout, not the old one.
	at := now.Add(21 * time.Second)
	dl, ok := m.MinDeadline("t1_inc_dns", []string{"9.9.9.9"}, at)
	if !ok || !dl.Equal(now.Add(20*time.Second).Add(300*time.Second)) {
		t.Fatalf("MinDeadline = %v (ok=%v), want deadline at refresh+300s", dl, ok)
	}
	// Unknown IPs and lapsed deadlines report false.
	if _, ok := m.MinDeadline("t1_inc_dns", []string{"1.2.3.4"}, at); ok {
		t.Fatal("MinDeadline found an unknown ip")
	}
	if _, ok := m.MinDeadline("t1_inc_dns", []string{"9.9.9.9"}, now.Add(400*time.Second)); ok {
		t.Fatal("MinDeadline found an expired element")
	}
	if _, ok := m.MinDeadline("t_missing", []string{"9.9.9.9"}, at); ok {
		t.Fatal("MinDeadline found an unknown set")
	}
}

// ---- plugin-level keeper flow ----------------------------------------------

// A keepalive tenant: a client query learns the domain into nft AND the
// registry; a due record is re-resolved and its element extended.
func TestKeeperPassRefreshesDueDomain(t *testing.T) {
	ops := newMockNFT()
	st := testState()
	st.Tenants[0].DNSKeepAlive = true
	h := &countedHandler{ttl: 120, perName: map[string]string{"www.example.com": "9.9.9.9"}}
	st.Tenants[0].Upstreams = []string{startUDPServer(t, h)}
	f := newTestPlugin(t, st, ops)
	f.keeperFile = filepath.Join(t.TempDir(), "keeper.bin")

	rec, r := query("www.example.com", dns.TypeA, "192.168.100.5")
	if _, err := serve(f, rec, r); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if ops.addCount() != 1 {
		t.Fatalf("learn did not add: %d calls", ops.addCount())
	}
	k := f.keeperIfAny("tenant1")
	if k == nil || k.len() != 1 {
		t.Fatalf("keeper registry not populated: %+v", k)
	}
	sts := f.tenantStatuses()
	if ks := sts["tenant1"].Keeper; ks == nil || ks.Domains != 1 {
		t.Fatalf("status keeper section: %+v", sts["tenant1"].Keeper)
	}

	// Open the refresh window (deadline in 50s, 60s window) and let the
	// keeper pass re-resolve the domain.
	k.register("www.example.com", SetIncDNS, 120*time.Second, time.Now().Add(50*time.Second))
	before := h.hits()
	f.keeperPass(context.Background())
	waitFor(t, 3*time.Second, 20*time.Millisecond, "keeper refresh completed", func() bool {
		_, refreshes, _, _ := k.stats()
		return refreshes >= 1 && h.hits() > before
	})
	if ops.addCount() != 2 {
		t.Fatalf("refresh did not re-add the element: %d calls", ops.addCount())
	}
	if d := k.due(time.Now()); len(d) != 0 {
		t.Fatalf("record still due after refresh: %+v", d)
	}
	if _, err := os.Stat(f.keeperFile); err != nil {
		t.Fatalf("registry not persisted: %v", err)
	}
	// Keeper queries never show up as client queries.
	if sts = f.tenantStatuses(); sts["tenant1"].Queries != 1 {
		t.Fatalf("client queries = %d, want 1", sts["tenant1"].Queries)
	}
}

// NXDOMAIN on the keeper re-resolve drops the record; the element ages out
// on its own.
func TestKeeperPassNXDOMAINDropsRecord(t *testing.T) {
	ops := newMockNFT()
	st := testState()
	st.Tenants[0].DNSKeepAlive = true
	h := &countedHandler{ttl: 120, nxFor: map[string]bool{"gone.example": true}}
	st.Tenants[0].Upstreams = []string{startUDPServer(t, h)}
	f := newTestPlugin(t, st, ops)

	k := f.getKeeper("tenant1")
	k.register("gone.example", SetIncDNS, 120*time.Second, time.Now().Add(50*time.Second))
	f.keeperPass(context.Background())
	waitFor(t, 3*time.Second, 20*time.Millisecond, "record dropped", func() bool {
		return k.len() == 0
	})
	if ops.addCount() != 0 {
		t.Fatalf("nft touched for a dead domain: %d calls", ops.addCount())
	}
}

// Three consecutive upstream failures drop the record back to passive
// learning.
func TestKeeperPassDropsAfterFailures(t *testing.T) {
	ops := newMockNFT()
	st := testState()
	st.Tenants[0].DNSKeepAlive = true
	st.Tenants[0].Upstreams = []string{"127.0.0.1:1"} // nothing listens
	f := newTestPlugin(t, st, ops)

	k := f.getKeeper("tenant1")
	k.register("down.example", SetIncDNS, 120*time.Second, time.Now().Add(50*time.Second))
	for i := int64(1); i <= 2; i++ {
		f.keeperPass(context.Background())
		waitFor(t, 3*time.Second, 10*time.Millisecond, "failure counted", func() bool {
			_, _, errs, _ := k.stats()
			return errs >= i
		})
	}
	f.keeperPass(context.Background())
	waitFor(t, 3*time.Second, 10*time.Millisecond, "record dropped after 3 failures", func() bool {
		return k.len() == 0
	})
}

// The binary registry survives a plugin restart (new instance, same file).
func TestKeeperSurvivesRestartViaBinaryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keeper.bin")
	now := time.Now()
	f1 := New()
	k1 := f1.getKeeper("tenant1")
	k1.register("a.example", SetIncDNS, 300*time.Second, now.Add(50*time.Second))  // due on load
	k1.register("b.example", SetExcDNS, 600*time.Second, now.Add(500*time.Second)) // not due
	if err := f1.persistKeepers(path); err != nil {
		t.Fatalf("persist: %v", err)
	}

	f2 := New()
	if err := f2.loadKeepers(path, time.Now()); err != nil {
		t.Fatalf("load: %v", err)
	}
	k2 := f2.keeperIfAny("tenant1")
	if k2 == nil || k2.len() != 2 {
		t.Fatalf("registry after restart: %+v", k2)
	}
	var dueNames []string
	for _, d := range k2.due(time.Now()) {
		dueNames = append(dueNames, d.name)
	}
	if len(dueNames) != 1 || dueNames[0] != "a.example" {
		t.Fatalf("due after restart = %v, want [a.example]", dueNames)
	}
}

// Registries of tenants with dns_keepalive off are pruned and never show a
// keeper status section.
func TestKeeperPassPrunesDisabledTenants(t *testing.T) {
	ops := newMockNFT()
	st := testState() // tenant1 has dns_keepalive unset
	h := &countedHandler{ttl: 120, perName: map[string]string{"www.example.com": "9.9.9.9"}}
	st.Tenants[0].Upstreams = []string{startUDPServer(t, h)}
	f := newTestPlugin(t, st, ops)

	rec, r := query("www.example.com", dns.TypeA, "192.168.100.5")
	if _, err := serve(f, rec, r); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if f.keeperIfAny("tenant1") != nil {
		t.Fatal("registry created for a non-keepalive tenant")
	}
	k := f.getKeeper("tenant1")
	k.register("x.example", SetIncDNS, 120*time.Second, time.Now().Add(50*time.Second))
	f.keeperPass(context.Background())
	// The pass had time to prune: give the (empty) goroutine table a moment.
	waitFor(t, 2*time.Second, 10*time.Millisecond, "registry pruned", func() bool {
		return f.keeperIfAny("tenant1") == nil
	})
	if sts := f.tenantStatuses(); sts["tenant1"].Keeper != nil {
		t.Fatalf("keeper section in status of a non-keepalive tenant: %+v", sts["tenant1"].Keeper)
	}
}

// A registry persisted while keepalive was on is rewritten (emptied) once a
// pass prunes it, so no stale records linger in keeper.bin.
func TestKeeperPassRewritesFileAfterPrune(t *testing.T) {
	ops := newMockNFT()
	path := filepath.Join(t.TempDir(), "keeper.bin")

	// A previously persisted registry for a now-disabled tenant.
	f1 := New()
	k1 := f1.getKeeper("tenant1")
	k1.register("a.example", SetIncDNS, 300*time.Second, time.Now().Add(50*time.Second))
	if err := f1.persistKeepers(path); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() <= 10 {
		t.Fatalf("seed file wrong: %v %+v", err, fi)
	}

	f := newTestPlugin(t, testState(), ops) // tenant1: dns_keepalive off
	f.keeperFile = path
	if err := f.loadKeepers(path, time.Now()); err != nil {
		t.Fatalf("load: %v", err)
	}
	f.keeperPass(context.Background())
	waitFor(t, 2*time.Second, 10*time.Millisecond, "registry pruned", func() bool {
		return f.keeperIfAny("tenant1") == nil
	})
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() != 10 {
		t.Fatalf("keeper.bin not emptied after prune: %d bytes", fi.Size())
	}
}
