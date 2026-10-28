package fbtenant

import (
	"context"
	"os"
	"time"

	"github.com/coredns/coredns/plugin/pkg/log"
)

const (
	// maxNFTBackoff caps the resync backoff after nft errors.
	maxNFTBackoff = 60 * time.Second
	// baseNFTBackoff is the first backoff after an nft error.
	baseNFTBackoff = 5 * time.Second
	// defaultResyncInterval is used while no state is loaded.
	defaultResyncInterval = 60 * time.Second
)

// stateFileInfo tracks the last seen dns_state.json stat for change detection.
type stateFileInfo struct {
	modTime time.Time
	size    int64
	exists  bool
}

// startWatcher runs the background loop until ctx is cancelled or Stop is
// called: state-file polling every pollInterval (2s), periodic resync by
// resync_interval_sec with error backoff (up to 60s), status writes every
// statusInterval (5s) and after each resync, keeper domain refresh scans
// every keeperInterval (15s) (CONTRACTS §3–§4).
func (f *FBTenant) startWatcher(ctx context.Context) {
	f.stopCh = make(chan struct{})
	lastStat := f.statState()
	f.setNextResync(f.resyncInterval())
	if err := f.loadKeepers(f.keeperFile, time.Now()); err != nil {
		log.Warningf("fbtenant: load keeper registry: %v (starting empty)", err)
	}

	poll := f.pollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	status := f.statusInterval
	if status <= 0 {
		status = 5 * time.Second
	}
	keeper := f.keeperInterval
	if keeper <= 0 {
		keeper = keeperTick
	}
	pollTick := time.NewTicker(poll)
	statusTick := time.NewTicker(status)
	keeperTick := time.NewTicker(keeper)
	go func() {
		defer pollTick.Stop()
		defer statusTick.Stop()
		defer keeperTick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-f.stopCh:
				return
			case <-pollTick.C:
				f.pollState(&lastStat)
			case <-statusTick.C:
				if err := f.writeStatus(time.Now()); err != nil {
					log.Warningf("fbtenant: write status: %v", err)
				}
			case <-keeperTick.C:
				f.keeperPass(ctx)
			}
		}
	}()
}

// Stop terminates the background loop (idempotent) and persists the keeper
// registry best-effort.
func (f *FBTenant) Stop() {
	if f.keeperFile != "" {
		if err := f.persistKeepers(f.keeperFile); err != nil {
			log.Warningf("fbtenant: persist keeper registry on stop: %v", err)
		}
	}
	if f.stopCh == nil {
		return
	}
	select {
	case <-f.stopCh:
	default:
		close(f.stopCh)
	}
}

func (f *FBTenant) statState() stateFileInfo {
	fi, err := os.Stat(f.stateFile)
	if err != nil {
		return stateFileInfo{exists: false}
	}
	return stateFileInfo{modTime: fi.ModTime(), size: fi.Size(), exists: true}
}

func (f *FBTenant) resyncInterval() time.Duration {
	if cfg := f.currentCfg(); cfg != nil && cfg.state.ResyncIntervalSec > 0 {
		return time.Duration(cfg.state.ResyncIntervalSec) * time.Second
	}
	return defaultResyncInterval
}

// pollState checks the state file for changes and whether a resync is due.
func (f *FBTenant) pollState(last *stateFileInfo) {
	cur := f.statState()
	changed := cur.exists != last.exists ||
		(cur.exists && (!cur.modTime.Equal(last.modTime) || cur.size != last.size))

	if changed {
		if err := f.reloadState(last); err != nil {
			log.Warningf("fbtenant: reload state: %v", err)
		}
	}

	if f.resyncDue(time.Now()) {
		if err := f.resyncAll("periodic"); err != nil {
			// resyncAll already scheduled the backoff retry.
			return
		}
		f.setNextResync(f.resyncInterval())
	}
}

// resync scheduling on FBTenant (shared by the watcher and post-error kicks).

func (f *FBTenant) setNextResync(d time.Duration) {
	f.resyncMu.Lock()
	f.nextResyncAt = time.Now().Add(d)
	f.resyncMu.Unlock()
}

func (f *FBTenant) resyncDue(now time.Time) bool {
	f.resyncMu.Lock()
	defer f.resyncMu.Unlock()
	return !f.nextResyncAt.IsZero() && !now.Before(f.nextResyncAt)
}

// scheduleResyncSoon requests an early resync after an nft error, honouring
// the current backoff.
func (f *FBTenant) scheduleResyncSoon() {
	f.resyncMu.Lock()
	defer f.resyncMu.Unlock()
	if f.backoff <= 0 {
		f.backoff = baseNFTBackoff
	}
	earliest := time.Now().Add(f.backoff)
	if f.nextResyncAt.IsZero() || earliest.Before(f.nextResyncAt) {
		f.nextResyncAt = earliest
	}
}

// reloadState re-reads dns_state.json, swaps the config and resyncs mirrors.
// A generation change makes the resync mandatory (CONTRACTS §2).
func (f *FBTenant) reloadState(last *stateFileInfo) error {
	st, err := loadStateFile(f.stateFile)
	if err != nil {
		return err
	}
	cfg, err := prepare(st)
	if err != nil {
		return err
	}
	oldGen := int64(-1)
	if old := f.currentCfg(); old != nil {
		oldGen = old.generation
	}
	f.setConfig(cfg)
	if last != nil {
		*last = f.statState()
	}
	reason := "state-change"
	if cfg.generation != oldGen {
		reason = "generation-change"
		log.Infof("fbtenant: dns_state generation %d -> %d; resyncing mirrors", oldGen, cfg.generation)
	}
	return f.resyncAll(reason)
}

// resyncAll rebuilds all mirrors from nftables and refreshes the status file.
// On nft failure the next attempt is backed off (doubling, capped at 60s).
func (f *FBTenant) resyncAll(reason string) error {
	m := f.getMirrors()
	if m == nil {
		return nil
	}
	now := time.Now()
	err := m.Resync(now)
	if err != nil {
		log.Warningf("fbtenant: resync (%s) failed: %v", reason, err)
		f.resyncMu.Lock()
		if f.backoff <= 0 {
			f.backoff = baseNFTBackoff
		} else {
			f.backoff *= 2
			if f.backoff > maxNFTBackoff {
				f.backoff = maxNFTBackoff
			}
		}
		f.nextResyncAt = now.Add(f.backoff)
		f.resyncMu.Unlock()
	} else {
		log.Infof("fbtenant: resync (%s) ok", reason)
		f.resyncMu.Lock()
		f.backoff = 0
		f.resyncMu.Unlock()
	}
	if serr := f.writeStatus(now); serr != nil {
		log.Warningf("fbtenant: write status: %v", serr)
	}
	return err
}
