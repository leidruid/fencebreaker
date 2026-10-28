package fbtenant

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/coredns/coredns/plugin/pkg/log"
	"github.com/miekg/dns"
)

/*
	Domain keeper (CONTRACTS §3, dns_keepalive): learning is passive — an
	nft element lives exactly clamp(TTL) from the first learn and repeated
	client queries do not extend it — so after expiry the prerouting mark
	disappears instantly and clients that never re-query (OS cache, hardcoded
	IPs, own DoH) lose tenant routing. The keeper registers every learned
	domain of a keepalive tenant and proactively re-resolves it shortly
	before its elements expire, extending them through mirror.Refresh.

	The registries live on FBTenant (keyed by tenant ID) and survive config
	reloads like the mirrors do; they persist to a compact binary file next
	to dns_state.json so a service restart starts refreshing immediately
	instead of waiting for the first client query per domain.
*/

const (
	// keeperTick is the background scan interval (shortened in tests).
	keeperTick = 15 * time.Second
	// keeperConcurrency caps simultaneous keeper re-resolves.
	keeperConcurrency = 8
	// keeperQueryTimeout bounds one keeper re-resolve including retries.
	keeperQueryTimeout = 15 * time.Second
	// A record is refreshed this long before its deadline:
	// max(keeperWindowFloor, timeout/keeperWindowDiv).
	keeperWindowFloor = 60 * time.Second
	keeperWindowDiv   = 4
	// keeperMaxFails consecutive failed re-resolves drop a record back to
	// passive learning.
	keeperMaxFails = 3
	// keeperStaleDrop bounds how old a persisted deadline may be to load
	// (records older than this reference nobody's live elements).
	keeperStaleDrop = 24 * time.Hour

	keeperFileMagic   = "FBKP"
	keeperFileVersion = 1
	keeperKindInc     = 1
	keeperKindExc     = 2
	keeperMaxName     = 255
	keeperMaxTenantID = 64
)

// keepRecord is one registered domain of one tenant.
type keepRecord struct {
	kind     string // SetIncDNS / SetExcDNS
	timeout  time.Duration
	deadline time.Time
	fails    int
}

// keepDue is a record scheduled for a re-resolve.
type keepDue struct {
	name string
	kind string
}

// refreshAt returns when re-resolution should start for the record.
func refreshAt(r keepRecord) time.Time {
	window := r.timeout / keeperWindowDiv
	if window < keeperWindowFloor {
		window = keeperWindowFloor
	}
	return r.deadline.Add(-window)
}

// tenantKeeper is the per-tenant domain registry with its own counters
// (keeper activity never shows up in client query stats).
type tenantKeeper struct {
	mu          sync.Mutex
	records     map[string]keepRecord
	refreshes   int64
	errors      int64
	lastRefresh time.Time
	dirty       bool
}

func newTenantKeeper() *tenantKeeper {
	return &tenantKeeper{records: map[string]keepRecord{}}
}

// register adds or updates a domain; a fresh learn is evidence the domain
// is alive, so the failure counter resets.
func (k *tenantKeeper) register(name, kind string, timeout time.Duration, deadline time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.records[name] = keepRecord{kind: kind, timeout: timeout, deadline: deadline}
	k.dirty = true
}

// remove drops a domain from the registry (expired for good: NXDOMAIN,
// no upstreams or too many failures).
func (k *tenantKeeper) remove(name string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.records[name]; ok {
		delete(k.records, name)
		k.dirty = true
	}
}

// due returns the records whose re-resolve window has opened.
func (k *tenantKeeper) due(now time.Time) []keepDue {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []keepDue
	for name, r := range k.records {
		if !now.Before(refreshAt(r)) {
			out = append(out, keepDue{name: name, kind: r.kind})
		}
	}
	return out
}

// refreshed records a successful re-resolve: new deadline, counter reset.
func (k *tenantKeeper) refreshed(name string, timeout time.Duration, now time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	r, ok := k.records[name]
	if !ok {
		return
	}
	r.timeout, r.deadline, r.fails = timeout, now.Add(timeout), 0
	k.records[name] = r
	k.refreshes++
	k.lastRefresh = now
	k.dirty = true
}

// failed records a failed re-resolve; it reports whether the record has hit
// keeperMaxFails and must be dropped back to passive learning.
func (k *tenantKeeper) failed(name string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	r, ok := k.records[name]
	if !ok {
		return false
	}
	r.fails++
	k.errors++
	k.records[name] = r
	return r.fails >= keeperMaxFails
}

// noteError counts an error that is not the domain's fault (e.g. an nft
// write failure) without touching the failure counter.
func (k *tenantKeeper) noteError() {
	k.mu.Lock()
	k.errors++
	k.mu.Unlock()
}

func (k *tenantKeeper) len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.records)
}

func (k *tenantKeeper) isDirty() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.dirty
}

func (k *tenantKeeper) clearDirty() {
	k.mu.Lock()
	k.dirty = false
	k.mu.Unlock()
}

// stats snapshots the counters for plugin_status.json.
func (k *tenantKeeper) stats() (domains int, refreshes, errors int64, last time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.records), k.refreshes, k.errors, k.lastRefresh
}

// keeperFileRecord is the on-disk representation of one registered domain.
type keeperFileRecord struct {
	Tenant   string
	Name     string
	Kind     string
	Timeout  time.Duration
	Deadline time.Time
}

// snapshot flattens the registry for persistence.
func (k *tenantKeeper) snapshot(tenant string) []keeperFileRecord {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]keeperFileRecord, 0, len(k.records))
	for name, r := range k.records {
		out = append(out, keeperFileRecord{
			Tenant: tenant, Name: name, Kind: r.kind,
			Timeout: r.timeout, Deadline: r.deadline,
		})
	}
	return out
}

// restore loads one persisted record; stale deadlines (service was down
// long enough that no live element can remain) are dropped.
func (k *tenantKeeper) restore(rec keeperFileRecord, now time.Time) {
	if rec.Deadline.Before(now.Add(-keeperStaleDrop)) {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.records[rec.Name] = keepRecord{
		kind: rec.Kind, timeout: rec.Timeout, deadline: rec.Deadline,
	}
	k.dirty = true
}

// ---- binary codec -----------------------------------------------------------
//
// Layout (little-endian, CONTRACTS §3):
//
//	magic "FBKP" | u8 version | u8 flags | u32 count
//	count × [u16 nameLen name][u8 tenantLen tenant][u8 kind][u32 timeoutSec][i64 deadlineUnixSec]

var (
	keeperKindByte = map[string]byte{SetIncDNS: keeperKindInc, SetExcDNS: keeperKindExc}
	keeperByteKind = map[byte]string{keeperKindInc: SetIncDNS, keeperKindExc: SetExcDNS}
)

func encodeKeeperFile(w io.Writer, recs []keeperFileRecord) error {
	bw := bufio.NewWriter(w)
	head := make([]byte, 0, 12)
	head = append(head, keeperFileMagic...)
	head = append(head, keeperFileVersion, 0)
	head = binary.LittleEndian.AppendUint32(head, uint32(len(recs)))
	if _, err := bw.Write(head); err != nil {
		return err
	}
	rec := make([]byte, 0, 24+keeperMaxName+keeperMaxTenantID)
	for _, r := range recs {
		kind, ok := keeperKindByte[r.Kind]
		if !ok {
			return errors.New("keeper encode: unknown set kind " + r.Kind)
		}
		// Length guards keep the u16/u8 fields from silently truncating.
		if len(r.Name) == 0 || len(r.Name) > keeperMaxName {
			return fmt.Errorf("keeper encode: bad name length %d", len(r.Name))
		}
		if len(r.Tenant) == 0 || len(r.Tenant) > keeperMaxTenantID {
			return fmt.Errorf("keeper encode: bad tenant length %d", len(r.Tenant))
		}
		if r.Timeout < time.Second {
			return fmt.Errorf("keeper encode: timeout %v below 1s", r.Timeout)
		}
		rec = rec[:0]
		rec = binary.LittleEndian.AppendUint16(rec, uint16(len(r.Name)))
		rec = append(rec, r.Name...)
		rec = append(rec, uint8(len(r.Tenant)))
		rec = append(rec, r.Tenant...)
		rec = append(rec, kind)
		rec = binary.LittleEndian.AppendUint32(rec, uint32(r.Timeout/time.Second))
		rec = binary.LittleEndian.AppendUint64(rec, uint64(r.Deadline.Unix()))
		if _, err := bw.Write(rec); err != nil {
			return err
		}
	}
	return bw.Flush()
}

func decodeKeeperFile(r io.Reader) ([]keeperFileRecord, error) {
	br := bufio.NewReader(io.LimitReader(r, 8<<20))
	head := make([]byte, 10) // magic(4) version(1) flags(1) count(4)
	if _, err := io.ReadFull(br, head); err != nil {
		return nil, err
	}
	if string(head[:4]) != keeperFileMagic {
		return nil, errors.New("keeper decode: bad magic")
	}
	if head[4] != keeperFileVersion {
		return nil, fmt.Errorf("keeper decode: unsupported version %d", head[4])
	}
	count := binary.LittleEndian.Uint32(head[6:10])
	if int64(count)*int64(24+keeperMaxName+keeperMaxTenantID) > 64<<20 {
		return nil, errors.New("keeper decode: implausible record count")
	}
	out := make([]keeperFileRecord, 0, count)
	for i := uint32(0); i < count; i++ {
		var lenBuf [2]byte
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			return nil, err
		}
		nameLen := int(binary.LittleEndian.Uint16(lenBuf[:]))
		if nameLen == 0 || nameLen > keeperMaxName {
			return nil, fmt.Errorf("keeper decode: bad name length %d", nameLen)
		}
		name := make([]byte, nameLen)
		if _, err := io.ReadFull(br, name); err != nil {
			return nil, err
		}
		var one [1]byte
		if _, err := io.ReadFull(br, one[:]); err != nil {
			return nil, err
		}
		tenantLen := int(one[0])
		if tenantLen == 0 || tenantLen > keeperMaxTenantID {
			return nil, fmt.Errorf("keeper decode: bad tenant length %d", tenantLen)
		}
		tenant := make([]byte, tenantLen)
		if _, err := io.ReadFull(br, tenant); err != nil {
			return nil, err
		}
		var tail [13]byte // kind + u32 timeout + i64 deadline
		if _, err := io.ReadFull(br, tail[:]); err != nil {
			return nil, err
		}
		kind, ok := keeperByteKind[tail[0]]
		if !ok {
			return nil, fmt.Errorf("keeper decode: bad kind byte %d", tail[0])
		}
		timeout := time.Duration(binary.LittleEndian.Uint32(tail[1:5])) * time.Second
		if timeout <= 0 {
			return nil, errors.New("keeper decode: non-positive timeout")
		}
		deadline := time.Unix(int64(binary.LittleEndian.Uint64(tail[5:13])), 0).UTC()
		out = append(out, keeperFileRecord{
			Tenant: string(tenant), Name: string(name), Kind: kind,
			Timeout: timeout, Deadline: deadline,
		})
	}
	// Trailing garbage means a torn write; the atomic rename makes this
	// unlikely, but refuse silently-truncated registries all the same.
	if _, err := br.ReadByte(); err != io.EOF {
		return nil, errors.New("keeper decode: trailing data")
	}
	return out, nil
}

// ---- FBTenant integration ---------------------------------------------------

func (f *FBTenant) getKeeper(id string) *tenantKeeper {
	f.keeperMu.Lock()
	defer f.keeperMu.Unlock()
	if f.keepers == nil {
		f.keepers = map[string]*tenantKeeper{}
	}
	k, ok := f.keepers[id]
	if !ok {
		k = newTenantKeeper()
		f.keepers[id] = k
	}
	return k
}

func (f *FBTenant) keeperIfAny(id string) *tenantKeeper {
	f.keeperMu.Lock()
	defer f.keeperMu.Unlock()
	return f.keepers[id]
}

// pruneKeepers drops registries of tenants that no longer keep domains;
// it reports whether anything was dropped (the persisted file then needs a
// rewrite so a stale registry does not linger on disk).
func (f *FBTenant) pruneKeepers(enabled map[string]bool) bool {
	f.keeperMu.Lock()
	defer f.keeperMu.Unlock()
	deleted := false
	for id := range f.keepers {
		if !enabled[id] {
			delete(f.keepers, id)
			deleted = true
		}
	}
	return deleted
}

// keeperPathFor returns the keeper.bin path next to the state file.
func keeperPathFor(stateFile string) string {
	return filepath.Join(filepath.Dir(stateFile), "keeper.bin")
}

// loadKeepers populates the registries from the persisted binary file.
func (f *FBTenant) loadKeepers(path string, now time.Time) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	recs, err := decodeKeeperFile(bytes.NewReader(data))
	if err != nil {
		return err
	}
	for _, rec := range recs {
		k := f.getKeeper(rec.Tenant)
		k.restore(rec, now)
		k.clearDirty() // loaded state is already on disk
	}
	if len(recs) > 0 {
		log.Infof("fbtenant: keeper registry loaded: %d records", len(recs))
	}
	return nil
}

// persistKeepers atomically writes the binary registry (tmp + rename).
func (f *FBTenant) persistKeepers(path string) error {
	if path == "" {
		return nil
	}
	f.keeperMu.Lock()
	recs := make([]keeperFileRecord, 0, 16)
	for id, k := range f.keepers {
		recs = append(recs, k.snapshot(id)...)
	}
	f.keeperMu.Unlock()

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".keeper-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := encodeKeeperFile(tmp, recs); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// keeperPass scans all keepalive tenants and re-resolves due domains.
// It runs on the watcher loop; spawns bounded goroutines for the queries.
func (f *FBTenant) keeperPass(ctx context.Context) {
	cfg := f.currentCfg()
	if cfg == nil {
		return
	}
	m := f.getMirrors()
	if m == nil {
		return
	}
	now := time.Now()
	enabled := map[string]bool{}
	for _, t := range cfg.tenants {
		if !t.keepalive {
			continue
		}
		enabled[t.raw.ID] = true
		k := f.getKeeper(t.raw.ID)
		for _, d := range k.due(now) {
			sem := f.keeperSem
			if sem == nil {
				continue
			}
			select {
			case sem <- struct{}{}:
				go func(t *preparedTenant, d keepDue) {
					defer func() { <-sem }()
					f.refreshKeeperDomain(ctx, cfg, t, d)
				}(t, d)
			default:
				// Concurrency cap reached: the next tick retries.
			}
		}
	}
	pruned := f.pruneKeepers(enabled)

	dirty := pruned
	f.keeperMu.Lock()
	for _, k := range f.keepers {
		if k.isDirty() {
			dirty = true
			break
		}
	}
	f.keeperMu.Unlock()
	if !dirty {
		return
	}
	if err := f.persistKeepers(f.keeperFile); err != nil {
		log.Warningf("fbtenant: persist keeper registry: %v", err)
		return
	}
	f.keeperMu.Lock()
	for _, k := range f.keepers {
		k.clearDirty()
	}
	f.keeperMu.Unlock()
}

// refreshKeeperDomain re-resolves one kept domain through the same upstream
// selection (forward zone, policy, health, mark) as client queries.
func (f *FBTenant) refreshKeeperDomain(ctx context.Context, cfg *config, t *preparedTenant, d keepDue) {
	k := f.keeperIfAny(t.raw.ID)
	if k == nil {
		return
	}
	m := f.getMirrors()
	setName := t.setName(d.kind)
	if m == nil || setName == "" {
		k.remove(d.name)
		return
	}
	mark := uint32(0)
	if t.raw.DNSViaTunnel {
		mark = t.raw.Mark
	}
	servers := t.forwards.lookup(d.name)
	if len(servers) == 0 {
		servers = t.upstreams
	}
	if len(servers) == 0 {
		k.remove(d.name)
		return
	}

	qctx, cancel := context.WithTimeout(ctx, keeperQueryTimeout)
	defer cancel()
	resp, err := f.client.Resolve(qctx, servers, t.upstreamPolicy, d.name, dns.TypeA, mark)
	now := time.Now()
	if err != nil {
		if drop := k.failed(d.name); drop {
			k.remove(d.name)
		}
		return
	}
	// NXDOMAIN or an empty answer means the domain is gone: stop keeping
	// it, its elements age out on their own.
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) == 0 {
		k.remove(d.name)
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
		if tt := time.Duration(a.Hdr.Ttl) * time.Second; qttl == 0 || tt < qttl {
			qttl = tt
		}
	}
	if len(ips) == 0 {
		k.remove(d.name)
		return
	}
	timeout := clampTTL(qttl, ttlMin(&cfg.state), ttlMax(&cfg.state))
	if err := m.Refresh(setName, ips, timeout, now); err != nil {
		// An nft failure is not the domain's fault: count the error but
		// keep the record; the resync reconciles the mirror.
		k.noteError()
		log.Warningf("fbtenant: keeper refresh %s tenant %s: %v", d.name, t.raw.ID, err)
		f.scheduleResyncSoon()
		return
	}
	k.refreshed(d.name, timeout, now)
}
