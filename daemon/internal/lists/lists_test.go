package lists

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leidruid/fencebreaker/daemon/internal/model"
)

func TestParseMixed(t *testing.T) {
	p := Parse([]byte(`
# comment
1.2.3.4/24
9.9.9.9
example.com
Sub.Example.COM.
not a domain!
!!bad
5.6.7.8/24,
`))
	if len(p.Nets) != 3 {
		t.Fatalf("nets = %v", p.Nets)
	}
	if p.Nets[0] != "1.2.3.0/24" || p.Nets[1] != "9.9.9.9/32" || p.Nets[2] != "5.6.7.0/24" {
		t.Fatalf("nets canonicalization wrong: %v", p.Nets)
	}
	if len(p.Domains) != 2 || p.Domains[0] != "example.com" || p.Domains[1] != "sub.example.com" {
		t.Fatalf("domains = %v", p.Domains)
	}
}

func TestManagerRefreshAndSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("10.1.0.0/16\nexample.org\n"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	m := New(dir)
	def := &model.ListDef{Name: "bl", URL: srv.URL, RefreshH: 24}
	m.LoadAll([]*model.ListDef{def})

	if err := m.Refresh(t.Context(), def); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	if len(snap["bl"].Nets) != 1 || snap["bl"].Nets[0] != "10.1.0.0/16" {
		t.Fatalf("snapshot nets = %v", snap["bl"].Nets)
	}
	if len(snap["bl"].Domains) != 1 {
		t.Fatalf("snapshot domains = %v", snap["bl"].Domains)
	}
	if _, err := os.Stat(filepath.Join(dir, "bl.txt")); err != nil {
		t.Fatal("raw cache not written")
	}
	if m.DueForRefresh(def) {
		t.Fatal("fresh list must not be due")
	}
	// force expiry
	m.mu.Lock()
	m.entries["bl"].Updated = time.Now().Add(-25 * time.Hour)
	m.mu.Unlock()
	if !m.DueForRefresh(def) {
		t.Fatal("expired list must be due")
	}
	st := m.Statuses()
	if len(st) != 1 || st[0].State != "ok" || st[0].Nets != 1 {
		t.Fatalf("statuses = %+v", st)
	}
}

func TestLoadAllFromCache(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bl.txt"), []byte("1.1.1.0/24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(dir)
	m.LoadAll([]*model.ListDef{{Name: "bl", URL: "http://x"}})
	if len(m.Snapshot()["bl"].Nets) != 1 {
		t.Fatal("cache load failed")
	}
}

func TestManualList(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	def := &model.ListDef{
		Name:    "manual",
		URL:     "",
		Content: "# hand-made\n192.0.2.0/24\nexample.net\nexample.net\nbad line!\n",
	}

	m.LoadAll([]*model.ListDef{def})
	snap := m.Snapshot()
	if len(snap["manual"].Nets) != 1 || snap["manual"].Nets[0] != "192.0.2.0/24" {
		t.Fatalf("manual nets = %v", snap["manual"].Nets)
	}
	if len(snap["manual"].Domains) != 1 || snap["manual"].Domains[0] != "example.net" {
		t.Fatalf("manual domains = %v", snap["manual"].Domains)
	}
	if _, err := os.Stat(filepath.Join(dir, "manual.txt")); err == nil {
		t.Fatal("manual list must not write a raw cache")
	}

	if m.DueForRefresh(def) {
		t.Fatal("manual list must never be due for refresh")
	}
	if err := m.Refresh(t.Context(), def); err != nil {
		t.Fatalf("manual refresh: %v", err)
	}

	// editing the content option and re-applying must re-parse the list
	def.Content = "198.51.100.7\n"
	m.LoadAll([]*model.ListDef{def})
	snap = m.Snapshot()
	if len(snap["manual"].Nets) != 1 || snap["manual"].Nets[0] != "198.51.100.7/32" {
		t.Fatalf("re-parsed manual nets = %v", snap["manual"].Nets)
	}

	st := m.Statuses()
	if len(st) != 1 || st[0].State != "ok" {
		t.Fatalf("statuses = %+v", st)
	}
}
