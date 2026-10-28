package netres

import (
	"net"
	"testing"

	"github.com/leidruid/fencebreaker/daemon/internal/uci"
)

func fakeLookup(table map[string][]string) AddrLookup {
	return func(dev string) ([]net.Addr, error) {
		var out []net.Addr
		for _, c := range table[dev] {
			_, n, err := net.ParseCIDR(c)
			if err != nil {
				continue
			}
			out = append(out, n)
		}
		return out, nil
	}
}

func TestResolve(t *testing.T) {
	netcfg, err := uci.Parse([]byte(`
config interface 'guest'
	option device 'br-guest'
	option proto 'static'

config interface 'wan2'
	option device 'eth2'
	option proto 'dhcp'
`))
	if err != nil {
		t.Fatal(err)
	}
	r := New(netcfg, fakeLookup(map[string][]string{
		"br-guest": {"192.168.2.1/24", "fd00::1/64"},
		"eth2":     {"100.64.0.2/24"},
	}))
	if got := r.Resolve("guest"); len(got) != 1 || got[0] != "192.168.2.0/24" {
		t.Fatalf("guest = %v", got)
	}
	if got := r.Resolve("br-guest"); len(got) != 1 || got[0] != "192.168.2.0/24" {
		t.Fatalf("br-guest = %v", got)
	}
	if got := r.Resolve("10.7.0.0/16"); len(got) != 1 || got[0] != "10.7.0.0/16" {
		t.Fatalf("cidr = %v", got)
	}
	if got := r.Resolve("nosuch"); got != nil {
		t.Fatalf("nosuch = %v", got)
	}
	all := r.ResolveAll([]string{"guest", "br-guest"})
	if len(all) != 1 {
		t.Fatalf("ResolveAll dedup failed: %v", all)
	}
}
