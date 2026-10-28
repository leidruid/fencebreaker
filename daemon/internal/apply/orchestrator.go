// Package apply orchestrates a full configuration apply: UCI → model →
// nftables + routing tables + dns_state.json + sing-box config + services.
package apply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/leidruid/fencebreaker/daemon/internal/dnsgen"
	"github.com/leidruid/fencebreaker/daemon/internal/geodata"
	"github.com/leidruid/fencebreaker/daemon/internal/lists"
	"github.com/leidruid/fencebreaker/daemon/internal/model"
	"github.com/leidruid/fencebreaker/daemon/internal/netres"
	"github.com/leidruid/fencebreaker/daemon/internal/nftapply"
	"github.com/leidruid/fencebreaker/daemon/internal/nftgen"
	"github.com/leidruid/fencebreaker/daemon/internal/routing"
	"github.com/leidruid/fencebreaker/daemon/internal/singbox"
	"github.com/leidruid/fencebreaker/daemon/internal/status"
	"github.com/leidruid/fencebreaker/daemon/internal/uci"
)

// Paths locates every file the orchestrator touches.
type Paths struct {
	Config   string // /etc/config/fencebreaker
	Network  string // /etc/config/network
	RunDir   string // /var/run/fencebreaker
	ListsDir string // /etc/fencebreaker/lists
	Geodata  string // /etc/fencebreaker/geoip.dat
}

// DefaultPaths returns the production paths.
func DefaultPaths() Paths {
	return Paths{
		Config:   "/etc/config/fencebreaker",
		Network:  "/etc/config/network",
		RunDir:   "/var/run/fencebreaker",
		ListsDir: "/etc/fencebreaker/lists",
		Geodata:  "/etc/fencebreaker/geoip.dat",
	}
}

// Runner executes system commands (init scripts, conntrack).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
}

// ExecRunner runs commands for real.
type ExecRunner struct{}

// Run implements Runner.
func (ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Run()
}

// Applied is the persisted state of the last successful apply.
type Applied struct {
	Generation   uint64              `json:"generation"`
	TenantSets   map[string][2]string `json:"tenant_sets"`
	SingboxHash  string              `json:"singbox_hash"`
	CorefileHash string              `json:"corefile_hash"`
	SavedAt      time.Time           `json:"saved_at"`
}

// Orchestrator wires the subsystems together.
type Orchestrator struct {
	Paths   Paths
	Nft     *nftapply.Client
	Routes  *routing.Manager
	Cmd     Runner
	Lists   *lists.Manager
	Status  *status.Writer
	Log     Logger
	Started time.Time

	applied Applied
	geo     geodata.Countries
	geoMeta status.Geodata
}

// Logger is a minimal logging interface (log.Logger satisfies it).
type Logger interface {
	Printf(format string, v ...any)
}

// New creates the orchestrator.
func New(p Paths, cmd Runner, lg Logger) *Orchestrator {
	return &Orchestrator{
		Paths:  p,
		Nft:    nftapply.New(nftapply.ExecBackend{}),
		Routes: routing.New(),
		Cmd:    cmd,
		Lists:  lists.New(p.ListsDir),
		Status: status.New(p.RunDir + "/status.json"),
		Log:    lg,
	}
}

// LoadApplied restores the persisted apply state.
func (o *Orchestrator) LoadApplied() error {
	data, err := os.ReadFile(o.Paths.RunDir + "/applied.json")
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &o.applied)
}

func (o *Orchestrator) saveApplied() error {
	o.applied.SavedAt = time.Now()
	data, err := json.MarshalIndent(o.applied, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.Paths.RunDir, 0o755); err != nil {
		return err
	}
	tmp := o.Paths.RunDir + "/applied.json.tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, o.Paths.RunDir+"/applied.json")
}

// Full re-applies the whole configuration.
func (o *Orchestrator) Full(ctx context.Context) error {
	started := time.Now()
	cfgDoc, err := uci.ParseFile(o.Paths.Config)
	if err != nil {
		return o.failStatus(err, "parse config")
	}
	cfg, err := model.FromUCI(cfgDoc)
	if err != nil {
		return o.failStatus(err, "build model")
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		err := fmt.Errorf("invalid config: %s", joinErrs(errs))
		return o.failStatus(err, "validate")
	}

	if !cfg.Global.Enabled {
		return o.teardown(ctx)
	}

	// resolve tenant networks → subnets
	netDoc, err := uci.ParseFile(o.Paths.Network)
	if err != nil {
		o.Log.Printf("network config unreadable (%v); device-name matching only", err)
		netDoc = &uci.Config{}
	}
	res := netres.New(netDoc, routing.LinkAddrFunc)
	srcNets := map[string][]string{}
	tenants := cfg.EnabledTenants()
	for _, t := range tenants {
		cidrs := res.ResolveAll(t.Networks)
		if len(cidrs) == 0 {
			o.Log.Printf("tenant %s: networks %v resolved to no subnets yet", t.Name, t.Networks)
		}
		srcNets[t.ID] = cidrs
	}

	// lists
	o.Lists.LoadAll(cfg.Lists)
	for _, def := range cfg.Lists {
		if o.Lists.DueForRefresh(def) {
			if err := o.Lists.Refresh(ctx, def); err != nil {
				o.Log.Printf("list %s refresh failed: %v", def.Name, err)
			}
		}
	}
	parsedLists := o.Lists.Snapshot()

	// geodata
	o.ensureGeodata(ctx, cfg)
	geoUsed := map[string][]string{}
	for _, t := range tenants {
		for _, cc := range append(append([]string{}, t.IncludeGeoIP...), t.ExcludeGeoIP...) {
			cc = strings.ToLower(cc)
			if _, ok := o.geo[cc]; ok {
				geoUsed[cc] = o.geo[cc]
			}
		}
	}

	// nftables: dump learned DNS sets, then recreate the table
	var setNames []string
	for _, t := range tenants {
		setNames = append(setNames, t.Sets().IncDNS, t.Sets().ExcDNS)
	}
	dumped, err := o.Nft.DumpSets(ctx, setNames)
	if err != nil {
		o.Log.Printf("nft dump failed: %v (continuing without restore)", err)
		dumped = map[string][]nftapply.Element{}
	}
	curSets := map[string][2]string{}
	for _, t := range tenants {
		s := t.Sets()
		curSets[t.ID] = [2]string{s.IncDNS, s.ExcDNS}
	}
	mappingChanged := dnsgen.MappingChanged(&dnsgen.State{Tenants: tenantStates(curSets)}, o.applied.TenantSets)
	restore := map[string][]nftgen.Elem{}
	if !mappingChanged {
		for name, elems := range dumped {
			var list []nftgen.Elem
			for _, e := range elems {
				if to := nftapply.RestoreTimeout(e); to > 0 {
					list = append(list, nftgen.Elem{IP: e.IP, TimeoutSec: to})
				}
			}
			if len(list) > 0 {
				restore[name] = list
			}
		}
	}

	ruleset := nftgen.Render(&nftgen.Inputs{
		Config:     cfg,
		Lists:      parsedLists,
		GeoIP:      geoUsed,
		DNSRestore: restore,
		SrcNets:    srcNets,
	})
	if ruleset == "" {
		if err := o.Nft.DeleteTable(ctx); err != nil {
			o.Log.Printf("nft delete table: %v", err)
		}
	} else {
		if err := o.Nft.DeleteTable(ctx); err != nil {
			o.Log.Printf("nft delete table: %v", err)
		}
		if err := o.Nft.Apply(ctx, ruleset); err != nil {
			return o.failStatus(err, "apply nft ruleset")
		}
	}

	// routing
	o.Routes.Remember(tenants)
	if err := o.Routes.Reconcile(ctx); err != nil {
		o.Log.Printf("routing reconcile: %v", err)
	}

	// dns state
	dnsState := dnsgen.Build(cfg, srcNets, parsedLists)
	if mappingChanged {
		o.applied.Generation++
	}
	dnsState.Generation = o.applied.Generation
	if err := dnsgen.Write(o.Paths.RunDir+"/dns_state.json", dnsState); err != nil {
		return o.failStatus(err, "write dns_state.json")
	}

	// corefile (static; plugin watches dns_state.json)
	corefile := fmt.Sprintf(".:%d {\n    fbtenant {\n        state %s/dns_state.json\n    }\n    errors\n}\n",
		cfg.Global.CoreDNSPort, o.Paths.RunDir)
	if err := o.writeAndRestart(ctx, o.Paths.RunDir+"/Corefile", corefile, "fencebreaker-coredns", &o.applied.CorefileHash); err != nil {
		o.Log.Printf("coredns service: %v", err)
	}

	// sing-box
	sbCfg, needSingbox := singbox.Build(cfg)
	if needSingbox {
		if err := o.writeAndRestart(ctx, o.Paths.RunDir+"/sing-box.json", sbCfg, "fencebreaker-singbox", &o.applied.SingboxHash); err != nil {
			o.Log.Printf("sing-box service: %v", err)
		}
	} else if o.applied.SingboxHash != "" {
		o.stopService(ctx, "fencebreaker-singbox")
		o.applied.SingboxHash = ""
	}

	// sysctl + conntrack hygiene
	for _, t := range tenants {
		if t.EgressMode == model.EgressInterface {
			setRPFilter(t.EgressInterface)
		}
		o.flushConntrack(ctx, t.Mark())
	}

	o.applied.TenantSets = curSets
	if err := o.saveApplied(); err != nil {
		o.Log.Printf("save applied state: %v", err)
	}
	o.writeStatus(cfg, started)
	o.Log.Printf("full apply done in %s (tenants=%d gen=%d)", time.Since(started).Round(time.Millisecond), len(tenants), o.applied.Generation)
	return nil
}

// RoutesOnly reconciles routing after netlink events (cheap path).
func (o *Orchestrator) RoutesOnly(ctx context.Context) error {
	if err := o.Routes.Reconcile(ctx); err != nil {
		o.Log.Printf("routing reconcile: %v", err)
		return err
	}
	return nil
}

// RefreshDue refreshes URL lists whose interval elapsed; it reports whether
// anything changed and a full apply is needed. It also re-downloads geodata
// when due (the new file is picked up by the next Full).
func (o *Orchestrator) RefreshDue(ctx context.Context) bool {
	cfgDoc, err := uci.ParseFile(o.Paths.Config)
	if err != nil {
		return false
	}
	cfg, err := model.FromUCI(cfgDoc)
	if err != nil {
		return false
	}
	o.Lists.LoadAll(cfg.Lists)
	changed := false
	for _, def := range cfg.Lists {
		if o.Lists.DueForRefresh(def) {
			if err := o.Lists.Refresh(ctx, def); err != nil {
				o.Log.Printf("list %s refresh failed: %v", def.Name, err)
				continue
			}
			changed = true
		}
	}
	return changed
}

func (o *Orchestrator) teardown(ctx context.Context) error {
	if err := o.Routes.Flush(ctx); err != nil {
		o.Log.Printf("routing flush: %v", err)
	}
	if err := o.Nft.DeleteTable(ctx); err != nil {
		o.Log.Printf("nft delete table: %v", err)
	}
	o.stopService(ctx, "fencebreaker-singbox")
	o.stopService(ctx, "fencebreaker-coredns")
	o.applied = Applied{}
	_ = o.saveApplied()
	o.Log.Printf("fencebreaker disabled: cleaned up")
	return nil
}

func (o *Orchestrator) ensureGeodata(ctx context.Context, cfg *model.Config) {
	needDownload := false
	if fi, err := os.Stat(o.Paths.Geodata); err != nil {
		needDownload = true
	} else if cfg.Global.GeodataRefreshH > 0 && time.Since(fi.ModTime()) > time.Duration(cfg.Global.GeodataRefreshH)*time.Hour {
		needDownload = true
	}
	if needDownload && cfg.Global.GeodataURL != "" {
		if err := geodata.Download(ctx, cfg.Global.GeodataURL, o.Paths.Geodata); err != nil {
			o.geoMeta = status.Geodata{State: "error", Error: err.Error()}
			o.Log.Printf("geodata download failed: %v", err)
		}
	}
	c, err := geodata.ParseFile(o.Paths.Geodata)
	if err != nil {
		if o.geoMeta.State != "error" {
			o.geoMeta = status.Geodata{State: "missing", Error: err.Error()}
		}
		o.geo = nil
		return
	}
	o.geo = c
	total := 0
	for _, v := range c {
		total += len(v)
	}
	if fi, err := os.Stat(o.Paths.Geodata); err == nil {
		o.geoMeta = status.Geodata{State: "ok", Updated: fi.ModTime(), Entries: total}
	}
}

func (o *Orchestrator) writeAndRestart(ctx context.Context, path, content, service string, hash *string) error {
	sum := sha256.Sum256([]byte(content))
	newHash := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(o.Paths.RunDir, 0o755); err != nil {
		return err
	}
	if *hash == newHash {
		// unchanged config: make sure the service is running
		return o.Cmd.Run(ctx, "/etc/init.d/"+service, "start")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := o.Cmd.Run(ctx, "/etc/init.d/"+service, "restart"); err != nil {
		return err
	}
	*hash = newHash
	return nil
}

func (o *Orchestrator) stopService(ctx context.Context, service string) {
	if err := o.Cmd.Run(ctx, "/etc/init.d/"+service, "stop"); err != nil {
		o.Log.Printf("stop %s: %v", service, err)
	}
}

func (o *Orchestrator) flushConntrack(ctx context.Context, mark uint32) {
	if _, err := exec.LookPath("conntrack"); err != nil {
		return
	}
	_ = o.Cmd.Run(ctx, "conntrack", "-D", "-m", fmt.Sprintf("0x%x", mark))
}

func setRPFilter(iface string) {
	if iface == "" {
		return
	}
	path := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/rp_filter", iface)
	_ = os.WriteFile(path, []byte("2"), 0o644)
}

func (o *Orchestrator) failStatus(err error, stage string) error {
	o.Log.Printf("%s: %v", stage, err)
	doc := &status.Document{
		Version: 1,
		Updated: time.Now(),
		Daemon:  status.Daemon{UptimeSec: int64(time.Since(o.Started).Seconds()), LastError: err.Error()},
	}
	_ = o.Status.Write(doc)
	return fmt.Errorf("%s: %w", stage, err)
}

// writeStatus assembles and stores the current status.json.
func (o *Orchestrator) writeStatus(cfg *model.Config, appliedAt time.Time) {
	doc := &status.Document{
		Version: 1,
		Updated: time.Now(),
		Daemon: status.Daemon{
			Generation: o.applied.Generation,
			UptimeSec:  int64(time.Since(o.Started).Seconds()),
			LastApply:  appliedAt,
			LastError:  "",
		},
		Tenants: status.TenantsFrom(cfg),
		Geodata: o.geoMeta,
	}
	for _, ls := range o.Lists.Statuses() {
		doc.Lists = append(doc.Lists, status.List{
			Name: ls.Name, State: ls.State, Updated: ls.Updated,
			Nets: ls.Nets, Domains: ls.Domains, Error: ls.Error,
		})
	}
	plugin, err := status.LoadPlugin(o.Paths.RunDir + "/plugin_status.json")
	if err == nil && plugin != nil {
		for i := range doc.Tenants {
			t := &doc.Tenants[i]
			if p, ok := plugin.Tenants[t.ID]; ok {
				t.DNS = status.DNSCounters{
					Queries:           p.Queries,
					CacheHits:         p.CacheHits,
					Learned:           p.Learned,
					DuplicatesSkipped: p.DuplicatesSkipped,
					UpstreamErrors:    p.UpstreamErrors,
					Keeper:            p.Keeper,
				}
				t.Sets.IncDNS = p.SetSizes.IncDNS
				if p.LastError != "" {
					t.Errors = append(t.Errors, p.LastError)
				}
			}
		}
	}
	sort.Slice(doc.Tenants, func(i, j int) bool { return doc.Tenants[i].Name < doc.Tenants[j].Name })
	_ = o.Status.Write(doc)
}

// WriteStatusNow refreshes status.json without re-applying.
func (o *Orchestrator) WriteStatusNow() {
	cfgDoc, err := uci.ParseFile(o.Paths.Config)
	if err != nil {
		return
	}
	cfg, err := model.FromUCI(cfgDoc)
	if err != nil {
		return
	}
	o.writeStatus(cfg, time.Now())
}

func joinErrs(errs []error) string {
	var parts []string
	for _, e := range errs {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "; ")
}

func tenantSetsForState(m map[string][2]string) []*dnsgen.TenantState {
	var out []*dnsgen.TenantState
	for id, s := range m {
		out = append(out, &dnsgen.TenantState{ID: id, Sets: dnsgen.Sets{IncDNS: s[0], ExcDNS: s[1]}})
	}
	return out
}

// tenantStates adapts the map for dnsgen.MappingChanged.
func tenantStates(m map[string][2]string) []*dnsgen.TenantState { return tenantSetsForState(m) }
