// Package netres resolves tenant network references (uci interface names,
// device names or literal CIDRs) into concrete IPv4 subnets. Uci interfaces
// are mapped to their device, and addresses come from a pluggable lookup so
// tests can run without netlink.
package netres

import (
	"net"
	"strings"

	"github.com/leidruid/fencebreaker/daemon/internal/uci"
)

// AddrLookup returns the IPv4 addresses of a link (device) by name.
type AddrLookup func(dev string) ([]net.Addr, error)

// Resolver resolves network references.
type Resolver struct {
	network  *uci.Config      // /etc/config/network
	linkAddr AddrLookup       // runtime addresses by device name
	extra    map[string][]net.IP // devices created outside uci (bridges etc.)
}

// New builds a resolver from the uci network config and an address lookup.
func New(network *uci.Config, lookup AddrLookup) *Resolver {
	return &Resolver{network: network, linkAddr: lookup}
}

// Resolve maps one reference to subnets. Supported forms: a CIDR, a uci
// interface name (static or dhcp), or a device name.
func (r *Resolver) Resolve(ref string) []string {
	ref = strings.TrimSpace(ref)
	if _, n, err := net.ParseCIDR(ref); err == nil {
		return []string{n.String()}
	}
	for _, dev := range r.interfaceDevices(ref) {
		if cidrs := r.deviceCIDRs(dev); len(cidrs) > 0 {
			return cidrs
		}
	}
	// last resort: treat the reference itself as a device name
	if cidrs := r.deviceCIDRs(ref); len(cidrs) > 0 {
		return cidrs
	}
	return nil
}

// ResolveAll resolves every reference, keeping order and deduplicating.
func (r *Resolver) ResolveAll(refs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range refs {
		for _, c := range r.Resolve(ref) {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// interfaceDevices returns device names for a uci interface section. A uci
// interface may reference a device directly, list several ports, or use a
// virtual name handled by netifd (e.g. @device sections); we cover the
// common shapes.
func (r *Resolver) interfaceDevices(name string) []string {
	if r.network == nil {
		return nil
	}
	var out []string
	for _, s := range r.network.SectionsOfType("interface") {
		if s.Name != name {
			continue
		}
		if d := s.Get("device", ""); d != "" {
			out = append(out, d)
		}
		out = append(out, s.List("ports")...)
		if ifname := s.Get("ifname", ""); ifname != "" {
			out = append(out, strings.Fields(ifname)...)
		}
	}
	return out
}

func (r *Resolver) deviceCIDRs(dev string) []string {
	if r.linkAddr == nil {
		return nil
	}
	addrs, err := r.linkAddr(dev)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if v4 := ipn.IP.To4(); v4 != nil {
				out = append(out, ipn.String())
			}
		}
	}
	return out
}
