package geodata

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// buildGeoIPList encodes a minimal GeoIPList protobuf for tests.
func buildGeoIPList(cc string, cidrs ...[2]any) []byte {
	var geoip []byte
	geoip = append(geoip, fieldStr(1, cc)...)
	for _, c := range cidrs {
		ip := c[0].(net.IP)
		prefix := uint64(c[1].(int))
		var cidrMsg []byte
		cidrMsg = append(cidrMsg, fieldBytes(1, ip)...)
		cidrMsg = append(cidrMsg, fieldVarint(2, prefix)...)
		geoip = append(geoip, fieldBytes(2, cidrMsg)...)
	}
	return fieldBytes(1, geoip) // GeoIPList.entry
}

func fieldStr(tag int, s string) []byte  { return fieldBytes(tag, []byte(s)) }
func fieldBytes(tag int, b []byte) []byte {
	out := []byte{byte(tag<<3 | 2)}
	out = appendVarint(out, uint64(len(b)))
	return append(out, b...)
}

func fieldVarint(tag int, v uint64) []byte {
	out := []byte{byte(tag<<3 | 0)}
	return appendVarint(out, v)
}

func appendVarint(b []byte, v uint64) []byte {
	var tmp [10]byte
	n := binary.PutUvarint(tmp[:], v)
	return append(b, tmp[:n]...)
}

func TestParseDat(t *testing.T) {
	dat := append(
		buildGeoIPList("RU", [2]any{net.ParseIP("45.135.240.0"), 22}, [2]any{net.ParseIP("5.45.112.0"), 20}),
		buildGeoIPList("CN", [2]any{net.ParseIP("1.2.4.0"), 24})...,
	)
	c, err := Parse(dat)
	if err != nil {
		t.Fatal(err)
	}
	if len(c["ru"]) != 2 || c["ru"][0] != "45.135.240.0/22" || c["ru"][1] != "5.45.112.0/20" {
		t.Fatalf("ru = %v", c["ru"])
	}
	if len(c["cn"]) != 1 || c["cn"][0] != "1.2.4.0/24" {
		t.Fatalf("cn = %v", c["cn"])
	}
}

func TestParseRoundtripFile(t *testing.T) {
	dat := buildGeoIPList("DE", [2]any{net.ParseIP("2.4.0.0"), 16})
	dir := t.TempDir()
	p := filepath.Join(dir, "geoip.dat")
	if err := os.WriteFile(p, dat, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c["de"]) != 1 || c["de"][0] != "2.4.0.0/16" {
		t.Fatalf("de = %v", c["de"])
	}
}

func TestParseTruncated(t *testing.T) {
	dat := buildGeoIPList("X", [2]any{net.ParseIP("1.1.1.1"), 8})
	if _, err := Parse(dat[:len(dat)-3]); err == nil {
		t.Fatal("expected truncation error")
	}
}
