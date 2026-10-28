package fbtenant

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeNFTBinary builds a shell script standing in for the nft binary: it
// appends every invocation's arguments to an args file and prints the dump
// file for `nft -j ...`.
func fakeNFTBinary(t *testing.T, dumpJSON string) (binary, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args.log")
	dumpFile := filepath.Join(dir, "dump.json")
	if err := os.WriteFile(dumpFile, []byte(dumpJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> "%s"
if [ "$1" = "-j" ]; then
  cat "%s"
fi
exit 0
`, argsFile, dumpFile)
	bin := filepath.Join(dir, "nft")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

// contractDump mimics `nft -j list set inet fencebreaker t1_inc_dns` output
// with all element encodings seen in the wild.
const contractDump = `{"nftables":[
  {"metainfo":{"version":"1.0.9"}},
  {"set":{"family":"inet","name":"t1_inc_dns","table":"fencebreaker","type":"ipv4_addr","flags":["timeout"],
    "elem":[
      {"elem":{"val":"1.2.3.4","timeout":300,"expiration":120}},
      "5.5.5.5",
      {"elem":{"val":"6.6.6.6"}},
      {"val":"7.7.7.7","timeout":60,"expiration":30}
    ]}},
  {"set":{"family":"inet","name":"other_set","table":"fencebreaker","elem":["8.8.8.8"]}}
]}`

func TestNFTListSetParsesDump(t *testing.T) {
	bin, _ := fakeNFTBinary(t, contractDump)
	ops := &nftOps{binary: bin}

	got, err := ops.ListSet("fencebreaker", "t1_inc_dns")
	if err != nil {
		t.Fatalf("ListSet: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("parsed elements: %v", got)
	}
	e := got["1.2.3.4"]
	if e.Remaining != 120*time.Second || e.Timeout != 300*time.Second {
		t.Fatalf("1.2.3.4 elem: %+v", e)
	}
	if e, ok := got["5.5.5.5"]; !ok || e.Timeout != 0 || e.Remaining != 0 {
		t.Fatalf("5.5.5.5 elem: %+v ok=%v", e, ok)
	}
	if e, ok := got["6.6.6.6"]; !ok || e.Remaining != 0 {
		t.Fatalf("6.6.6.6 elem: %+v ok=%v", e, ok)
	}
	e = got["7.7.7.7"]
	if e.Remaining != 30*time.Second || e.Timeout != 60*time.Second {
		t.Fatalf("7.7.7.7 elem: %+v", e)
	}
	if _, ok := got["8.8.8.8"]; ok {
		t.Fatal("element of another set leaked into the result")
	}
}

func TestNFTAddAndDeletePayloads(t *testing.T) {
	bin, argsFile := fakeNFTBinary(t, contractDump)
	ops := &nftOps{binary: bin}

	if _, err := ops.ListSet("fencebreaker", "t1_inc_dns"); err != nil {
		t.Fatalf("ListSet: %v", err)
	}
	err := ops.AddElements("fencebreaker", "t1_inc_dns", map[string]time.Duration{
		"9.9.9.9": 300 * time.Second,
		"4.4.4.4": 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("AddElements: %v", err)
	}
	err = ops.DeleteElements("fencebreaker", "t1_inc_dns", []string{"1.2.3.4", "5.5.5.5"})
	if err != nil {
		t.Fatalf("DeleteElements: %v", err)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	logged := string(data)
	if !strings.Contains(logged, "-j list set inet fencebreaker t1_inc_dns") {
		t.Fatalf("missing list invocation in %q", logged)
	}
	if !strings.Contains(logged, "add element inet fencebreaker t1_inc_dns") {
		t.Fatalf("missing add invocation in %q", logged)
	}
	if !strings.Contains(logged, "9.9.9.9 timeout 300s") || !strings.Contains(logged, "4.4.4.4 timeout 60s") {
		t.Fatalf("missing add payload in %q", logged)
	}
	if !strings.Contains(logged, "delete element inet fencebreaker t1_inc_dns") {
		t.Fatalf("missing delete invocation in %q", logged)
	}
	if !strings.Contains(logged, "1.2.3.4") || !strings.Contains(logged, "5.5.5.5") {
		t.Fatalf("missing delete payload in %q", logged)
	}
}

func TestNFTValidationRejectsBadInput(t *testing.T) {
	ops := newNFTOps()
	// Names are validated before the binary runs; a bogus binary path makes
	// any accidental exec fail loudly.
	ops.binary = "/nonexistent/nft"

	if err := ops.AddElements("bad;table", "set", map[string]time.Duration{"1.2.3.4": time.Second}); err == nil {
		t.Fatal("expected error for bad table name")
	}
	if err := ops.AddElements("fencebreaker", "bad set", map[string]time.Duration{"1.2.3.4": time.Second}); err == nil {
		t.Fatal("expected error for bad set name")
	}
	if err := ops.AddElements("fencebreaker", "s", map[string]time.Duration{"999.1.2.3": time.Second}); err == nil {
		t.Fatal("expected error for bad ip")
	}
	if err := ops.AddElements("fencebreaker", "s", map[string]time.Duration{"1.2.3.4": -time.Second}); err == nil {
		t.Fatal("expected error for bad timeout")
	}
	if err := ops.DeleteElements("fencebreaker", "s", []string{"no-ip"}); err == nil {
		t.Fatal("expected error for bad ip in delete")
	}
	// Empty input is a no-op without touching nft.
	if err := ops.AddElements("fencebreaker", "s", nil); err != nil {
		t.Fatalf("empty add should be a no-op: %v", err)
	}
	if err := ops.DeleteElements("fencebreaker", "s", nil); err != nil {
		t.Fatalf("empty delete should be a no-op: %v", err)
	}
}

func TestNFTListSetError(t *testing.T) {
	ops := newNFTOps()
	ops.binary = "/nonexistent/nft"
	if _, err := ops.ListSet("fencebreaker", "t1_inc_dns"); err == nil {
		t.Fatal("expected error when binary is missing")
	}
	if _, err := ops.ListSet("bad;table", "s"); err == nil {
		t.Fatal("expected validation error")
	}
}
