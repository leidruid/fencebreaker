package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leidruid/fencebreaker/daemon/internal/model"
	"github.com/leidruid/fencebreaker/daemon/internal/nftapply"
	"github.com/leidruid/fencebreaker/daemon/internal/status"
)

// fakeBackend records nft calls and can fail apply requests selectively.
type fakeBackend struct {
	calls []string // "<args>|<stdin>"
	// failApply, when set, is called for every "-f -" apply with the
	// ruleset text; a non-nil error fails the apply.
	failApply func(stdin string) error
}

func (b *fakeBackend) Run(ctx context.Context, args []string, stdin string) (string, error) {
	b.calls = append(b.calls, strings.Join(args, " ")+"|"+stdin)
	if len(args) > 0 && args[0] == "-f" && b.failApply != nil {
		if err := b.failApply(stdin); err != nil {
			return "", err
		}
	}
	// valid empty dump for "-j list set ..." calls
	return `{"nftables":[]}`, nil
}

type fakeRunner struct {
	cmds []string
}

func (r *fakeRunner) Run(ctx context.Context, name string, args ...string) error {
	r.cmds = append(r.cmds, name+" "+strings.Join(args, " "))
	return nil
}

// fakeRoutes keeps tests off netlink: real routing calls may block for a
// minute per dump on some kernels and need CAP_NET_ADMIN to mutate state.
type fakeRoutes struct {
	tenants []*model.Tenant
}

func (f *fakeRoutes) Remember(t []*model.Tenant) { f.tenants = t }
func (f *fakeRoutes) Tenants() []*model.Tenant   { return f.tenants }
func (f *fakeRoutes) Reconcile(_ context.Context) error {
	// mimic routing.Manager: mark every tenant's channel up
	for _, t := range f.tenants {
		t.EgressState = "up"
	}
	return nil
}
func (f *fakeRoutes) Flush(_ context.Context) error { return nil }

func writeTestConfig(t *testing.T, dir, extraTenantOpts string) {
	t.Helper()
	src := `
config fencebreaker 'global'
	option enabled '1'
	option coredns_port '1053'

config tenant
	option id 't1'
	option name 'Anon'
	option enabled '1'
	option egress_mode 'interface'
	option egress_interface 'wan2'
	option routing_mode 'select'
	option fallback 'main'
	list networks '172.16.100.0/24'
` + extraTenantOpts
	if err := os.WriteFile(filepath.Join(dir, "fencebreaker"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newOrch(t *testing.T, extraTenantOpts string) (*Orchestrator, *fakeBackend, string) {
	t.Helper()
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	writeTestConfig(t, dir, extraTenantOpts)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// an empty geoip.dat parses to zero countries; without it FromUCI
	// defaults GeodataURL and every Full downloads ~23 MB from GitHub
	if err := os.WriteFile(filepath.Join(dir, "geoip.dat"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{}
	o := New(Paths{
		Config:   filepath.Join(dir, "fencebreaker"),
		Network:  filepath.Join(dir, "network"), // absent: device matching skipped
		RunDir:   runDir,
		ListsDir: filepath.Join(dir, "lists"),
		Geodata:  filepath.Join(dir, "geoip.dat"),
	}, &fakeRunner{}, log.New(io.Discard, "", 0))
	o.Nft = nftapply.New(backend)
	o.Routes = &fakeRoutes{}
	return o, backend, runDir
}

func readStatus(t *testing.T, path string) *status.Document {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc status.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return &doc
}

func applies(backend *fakeBackend) []string {
	var out []string
	for _, c := range backend.calls {
		if rest, ok := strings.CutPrefix(c, "-f -|"); ok {
			out = append(out, rest)
		}
	}
	return out
}

func TestFullSavesLastGoodRuleset(t *testing.T) {
	o, _, runDir := newOrch(t, "")
	if err := o.Full(context.Background()); err != nil {
		t.Fatalf("full apply: %v", err)
	}
	if o.lastRuleset == "" {
		t.Fatal("lastRuleset is empty after a successful apply")
	}
	saved, err := os.ReadFile(filepath.Join(runDir, "ruleset.nft"))
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != o.lastRuleset {
		t.Error("ruleset.nft does not match the applied ruleset")
	}
	doc := readStatus(t, filepath.Join(runDir, "status.json"))
	if doc.Daemon.LastError != "" {
		t.Errorf("unexpected last_error after successful apply: %q", doc.Daemon.LastError)
	}
	if st := doc.Tenants[0].Egress.State; st != "up" {
		t.Errorf("egress state = %q, want up", st)
	}
	// the periodic refresh re-parses UCI and must keep the runtime state
	o.WriteStatusNow()
	doc = readStatus(t, filepath.Join(runDir, "status.json"))
	if st := doc.Tenants[0].Egress.State; st != "up" {
		t.Errorf("periodic status refresh lost egress state: %q", st)
	}
}

func TestApplyFailureRollsBackToLastGood(t *testing.T) {
	o, backend, runDir := newOrch(t, "")
	if err := o.Full(context.Background()); err != nil {
		t.Fatalf("full apply: %v", err)
	}
	good := o.lastRuleset

	// config change → the next ruleset differs from the last good one
	writeTestConfig(t, filepath.Dir(o.Paths.Config), "\tlist include_subnets '10.9.0.0/16'\n")
	backend.calls = nil
	backend.failApply = func(stdin string) error {
		if stdin != good {
			return fmt.Errorf("nft -f -: exit status 1: Error: Could not resolve hostname")
		}
		return nil
	}
	err := o.Full(context.Background())
	if err == nil || !strings.Contains(err.Error(), "apply nft ruleset") {
		t.Fatalf("expected apply failure, got %v", err)
	}

	got := applies(backend)
	if len(got) != 2 {
		t.Fatalf("expected failed apply + rollback apply, got %d applies", len(got))
	}
	if got[0] == good {
		t.Error("first apply must be the new (changed) ruleset")
	}
	if got[1] != good {
		t.Error("second apply must restore the last good ruleset")
	}

	// the failure must be visible in status.json and survive the
	// periodic status refresh
	if doc := readStatus(t, filepath.Join(runDir, "status.json")); doc.Daemon.LastError == "" {
		t.Fatal("last_error is empty after a failed apply")
	}
	o.WriteStatusNow()
	if doc := readStatus(t, filepath.Join(runDir, "status.json")); doc.Daemon.LastError == "" {
		t.Fatal("periodic status refresh wiped last_error")
	}

	// a later successful apply clears the error
	backend.failApply = nil
	if err := o.Full(context.Background()); err != nil {
		t.Fatalf("full apply: %v", err)
	}
	if doc := readStatus(t, filepath.Join(runDir, "status.json")); doc.Daemon.LastError != "" {
		t.Errorf("last_error not cleared after successful apply: %q", doc.Daemon.LastError)
	}
}

func TestRollbackWithoutSavedRulesetKeepsError(t *testing.T) {
	o, backend, runDir := newOrch(t, "")
	backend.failApply = func(stdin string) error {
		return fmt.Errorf("nft -f -: exit status 1: broken")
	}
	if err := o.Full(context.Background()); err == nil {
		t.Fatal("expected apply failure")
	}
	for _, c := range applies(backend) {
		_ = c
	}
	if n := len(applies(backend)); n != 1 {
		t.Fatalf("expected exactly one apply attempt, got %d", n)
	}
	if doc := readStatus(t, filepath.Join(runDir, "status.json")); doc.Daemon.LastError == "" {
		t.Fatal("last_error is empty after a failed first apply")
	}
}
