// Package geodata downloads and parses v2fly geoip.dat files into per-country
// CIDR lists. The protobuf wire format (GeoIPList/GeoIP/CIDR) is parsed with
// a small hand-rolled reader to avoid pulling in generated code.
package geodata

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/leidruid/fencebreaker/daemon/internal/httpx"
)

// MaxDownloadBytes caps the geoip.dat download.
const MaxDownloadBytes = 200 << 20

// Countries maps a lowercase country code to canonical CIDRs.
type Countries map[string][]string

// Download fetches url to dest atomically.
func Download(ctx context.Context, url, dest string) error {
	client := httpx.New(MaxDownloadBytes)
	data, err := client.Get(ctx, url)
	if err != nil {
		return err
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// ParseFile reads a geoip.dat file.
func ParseFile(path string) (Countries, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes the v2fly GeoIPList protobuf message.
func Parse(data []byte) (Countries, error) {
	out := Countries{}
	err := eachField(data, func(tag int, wire int, val []byte, varint uint64) error {
		if tag != 1 || wire != 2 {
			return nil
		}
		cc, cidrs, err := parseGeoIP(val)
		if err != nil {
			return err
		}
		if cc != "" && len(cidrs) > 0 {
			out[strings.ToLower(cc)] = cidrs
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseGeoIP decodes one GeoIP message: field 1 = country code (string),
// field 2 = CIDR submessage.
func parseGeoIP(data []byte) (string, []string, error) {
	var cc string
	var cidrs []string
	err := eachField(data, func(tag int, wire int, val []byte, varint uint64) error {
		switch {
		case tag == 1 && wire == 2:
			cc = string(val)
		case tag == 2 && wire == 2:
			ip, prefix, err := parseCIDRMsg(val)
			if err != nil {
				return err
			}
			if ip != nil {
				bits := 32
				if len(ip) == 16 {
					bits = 128
				}
				if prefix <= bits {
					cidrs = append(cidrs, fmt.Sprintf("%s/%d", ip.String(), prefix))
				}
			}
		}
		return nil
	})
	return cc, cidrs, err
}

// parseCIDRMsg decodes the CIDR message: field 1 = ip bytes, field 2 = prefix.
func parseCIDRMsg(data []byte) (net.IP, int, error) {
	var ip []byte
	var prefix int
	err := eachField(data, func(tag int, wire int, val []byte, varint uint64) error {
		switch {
		case tag == 1 && wire == 2:
			ip = append([]byte(nil), val...)
		case tag == 2 && wire == 0:
			prefix = int(varint)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if len(ip) != 4 && len(ip) != 16 {
		return nil, 0, nil
	}
	return net.IP(ip), prefix, nil
}

// eachField walks protobuf fields. For wire type 2 (LEN) val holds the bytes;
// for wire type 0 (VARINT) varint holds the number. Other wire types are
// skipped.
func eachField(data []byte, fn func(tag, wire int, val []byte, varint uint64) error) error {
	for i := 0; i < len(data); {
		key, n := uvarint(data[i:])
		if n <= 0 {
			return fmt.Errorf("geodata: bad field key at %d", i)
		}
		i += n
		tag := int(key >> 3)
		wire := int(key & 7)
		switch wire {
		case 0:
			v, n := uvarint(data[i:])
			if n <= 0 {
				return fmt.Errorf("geodata: bad varint at %d", i)
			}
			i += n
			if err := fn(tag, wire, nil, v); err != nil {
				return err
			}
		case 2:
			l, n := uvarint(data[i:])
			if n <= 0 {
				return fmt.Errorf("geodata: bad length at %d", i)
			}
			i += n
			if i+int(l) > len(data) {
				return fmt.Errorf("geodata: truncated field %d", tag)
			}
			if err := fn(tag, wire, data[i:i+int(l)], 0); err != nil {
				return err
			}
			i += int(l)
		case 1:
			if i+8 > len(data) {
				return fmt.Errorf("geodata: truncated fixed64")
			}
			i += 8
		case 5:
			if i+4 > len(data) {
				return fmt.Errorf("geodata: truncated fixed32")
			}
			i += 4
		default:
			return fmt.Errorf("geodata: unsupported wire type %d", wire)
		}
	}
	return nil
}

func uvarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}
