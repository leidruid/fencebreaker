package nftapply

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeBackend struct {
	calls   []string
	stdins  []string // parallel to calls
	outputs map[string]string // matched by joined args prefix
	errs    map[string]error
}

func (f *fakeBackend) Run(_ context.Context, args []string, stdin string) (string, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	f.stdins = append(f.stdins, stdin)
	if err, ok := f.errs[key]; ok {
		return "", err
	}
	if out, ok := f.outputs[key]; ok {
		return out, nil
	}
	return "", nil
}

func TestDeleteTableToleratesMissing(t *testing.T) {
	fb := &fakeBackend{errs: map[string]error{
		"delete table inet fencebreaker": errors.New("Error: cache initialization failed: No such file or directory"),
	}}
	c := New(fb)
	if err := c.DeleteTable(context.Background()); err != nil {
		t.Fatalf("missing table must not error: %v", err)
	}
	if err := c.DeleteTable(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestApplyPassesStdin(t *testing.T) {
	fb := &fakeBackend{}
	c := New(fb)
	if err := c.Apply(context.Background(), "table inet x {}"); err != nil {
		t.Fatal(err)
	}
	if len(fb.calls) != 1 || fb.calls[0] != "-f -" {
		t.Fatalf("calls = %v", fb.calls)
	}
	if fb.stdins[0] != "table inet x {}" {
		t.Fatalf("stdin = %q", fb.stdins[0])
	}
}

// Existing table: delete + ruleset must reach nft as ONE -f - transaction so
// there is no window where tenant traffic flows unmarked.
func TestApplyReplacingPrependsDelete(t *testing.T) {
	fb := &fakeBackend{}
	c := New(fb)
	if err := c.ApplyReplacing(context.Background(), "table inet fencebreaker {}"); err != nil {
		t.Fatal(err)
	}
	if len(fb.calls) != 2 || fb.calls[0] != "list table inet fencebreaker" || fb.calls[1] != "-f -" {
		t.Fatalf("calls = %v", fb.calls)
	}
	want := "delete table inet fencebreaker\ntable inet fencebreaker {}"
	if fb.stdins[1] != want {
		t.Fatalf("stdin = %q, want %q", fb.stdins[1], want)
	}
}

// Missing table: plain apply without the delete prelude (delete of a missing
// table would abort the whole transaction).
func TestApplyReplacingWithoutTable(t *testing.T) {
	fb := &fakeBackend{errs: map[string]error{
		"list table inet fencebreaker": errors.New("Error: No such file or directory"),
	}}
	c := New(fb)
	if err := c.ApplyReplacing(context.Background(), "table inet fencebreaker {}"); err != nil {
		t.Fatal(err)
	}
	if len(fb.calls) != 2 || fb.calls[1] != "-f -" {
		t.Fatalf("calls = %v", fb.calls)
	}
	if fb.stdins[1] != "table inet fencebreaker {}" {
		t.Fatalf("stdin = %q", fb.stdins[1])
	}
}

// A probe failure other than "missing table" must not fall through to a
// plain apply.
func TestApplyReplacingProbeError(t *testing.T) {
	fb := &fakeBackend{errs: map[string]error{
		"list table inet fencebreaker": errors.New("operation not permitted"),
	}}
	c := New(fb)
	if err := c.ApplyReplacing(context.Background(), "table inet fencebreaker {}"); err == nil {
		t.Fatal("expected probe error")
	}
	if len(fb.calls) != 1 {
		t.Fatalf("no -f - must run after a probe error: %v", fb.calls)
	}
}

func TestDumpSetsParsesJSON(t *testing.T) {
	fb := &fakeBackend{outputs: map[string]string{
		"-j list set inet fencebreaker t1_inc_dns": `{"nftables": [
			{"set": {"name": "t1_inc_dns", "elem": [
				{"elem": {"val": "1.2.3.4", "timeout": 300.0, "expires": 240.5}},
				{"elem": {"val": [5,6,7,8], "timeout": 86400.0, "expires": 80000.0}}
			]}}
		]}`,
	}}
	c := New(fb)
	sets, err := c.DumpSets(context.Background(), []string{"t1_inc_dns"})
	if err != nil {
		t.Fatal(err)
	}
	elems := sets["t1_inc_dns"]
	if len(elems) != 2 {
		t.Fatalf("elems = %+v", elems)
	}
	if elems[0].IP != "1.2.3.4" || elems[0].TimeoutSec != 300 || elems[0].RemainSec != 240 {
		t.Fatalf("elem0 = %+v", elems[0])
	}
	if elems[1].IP != "5.6.7.8" || elems[1].TimeoutSec != 86400 {
		t.Fatalf("elem1 = %+v", elems[1])
	}
	if r := RestoreTimeout(elems[0]); r != 240 {
		t.Fatalf("RestoreTimeout = %d", r)
	}
}

func TestDumpMissingSetIsNotError(t *testing.T) {
	fb := &fakeBackend{errs: map[string]error{
		"-j list set inet fencebreaker t1_inc_dns": errors.New("Error: No such file or directory"),
	}}
	c := New(fb)
	sets, err := c.DumpSets(context.Background(), []string{"t1_inc_dns"})
	if err != nil {
		t.Fatalf("missing set must not error: %v", err)
	}
	if len(sets["t1_inc_dns"]) != 0 {
		t.Fatalf("want empty, got %+v", sets["t1_inc_dns"])
	}
}
