// Package nftgen renders the complete nftables ruleset of the fencebreaker
// table from the runtime model. The ruleset is applied atomically by
// nftapply; this package is pure text generation and fully unit-testable.
package nftgen

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/leidruid/fencebreaker/daemon/internal/model"
)

// ParsedList is the parsed content of an URL-subscribed list.
type ParsedList struct {
	Nets    []string // canonical CIDRs
	Domains []string
}

// Elem is one restored nft set element with a timeout.
type Elem struct {
	IP         string
	TimeoutSec uint32
}

// Inputs carries everything the renderer needs.
type Inputs struct {
	Config     *model.Config
	Lists      map[string]*ParsedList // by list name
	GeoIP      map[string][]string    // country code → canonical CIDRs
	DNSRestore map[string][]Elem      // set name → elements to restore
	// SrcNets maps tenant ID → resolved source CIDRs (network names are
	// resolved to interface subnets by the netres package at runtime).
	SrcNets map[string][]string
}

// Render produces the nftables input file for table inet fencebreaker.
// Returns an empty string when there is nothing to install (no enabled
// tenants).
func Render(in *Inputs) string {
	tenants := in.Config.EnabledTenants()
	if len(tenants) == 0 {
		return ""
	}
	port := in.Config.Global.CoreDNSPort

	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("# rendered by fencebreakerd, do not edit")
	w("table inet fencebreaker {")

	// ---- sets ----
	for _, t := range tenants {
		s := t.Sets()
		renderAddrSet(&b, s.SrcNets, true, canonCIDRs(in.SrcNets[t.ID]))
		renderAddrSet(&b, s.HostsExc, false, canonIPs(t.ExcludedHosts))
		renderAddrSet(&b, s.HostsFull, false, canonIPs(t.FullHosts))

		var incStatic, excStatic []string
		incStatic = append(incStatic, canonCIDRs(t.IncludeSubnets)...)
		excStatic = append(excStatic, canonCIDRs(t.ExcludeSubnets)...)
		for _, name := range t.IncludeLists {
			if l := in.Lists[name]; l != nil {
				incStatic = append(incStatic, canonCIDRs(l.Nets)...)
			}
		}
		for _, name := range t.ExcludeLists {
			if l := in.Lists[name]; l != nil {
				excStatic = append(excStatic, canonCIDRs(l.Nets)...)
			}
		}
		var incGeo, excGeo []string
		for _, cc := range t.IncludeGeoIP {
			cidrs, ok := in.GeoIP[strings.ToLower(cc)]
			if !ok {
				w("# warning: geoip zone %q is not loaded", cc)
				continue
			}
			incGeo = append(incGeo, canonCIDRs(cidrs)...)
		}
		for _, cc := range t.ExcludeGeoIP {
			cidrs, ok := in.GeoIP[strings.ToLower(cc)]
			if !ok {
				w("# warning: geoip zone %q is not loaded", cc)
				continue
			}
			excGeo = append(excGeo, canonCIDRs(cidrs)...)
		}
		renderAddrSet(&b, s.IncStatic, true, dedup(incStatic))
		renderAddrSet(&b, s.ExcStatic, true, dedup(excStatic))
		renderAddrSet(&b, s.IncGeoIP, true, dedup(incGeo))
		renderAddrSet(&b, s.ExcGeoIP, true, dedup(excGeo))
		renderDNSSet(&b, s.IncDNS, in.Config.Global.TTLMax, in.DNSRestore[s.IncDNS])
		renderDNSSet(&b, s.ExcDNS, in.Config.Global.TTLMax, in.DNSRestore[s.ExcDNS])
		w("")
	}

	// ---- dns hijack ----
	w("\tchain dns_hijack {")
	// nat chains must sit above -200 (kernel constraint) and before fw4's
	// dstnat (-100)
	w("\t\ttype nat hook prerouting priority -150; policy accept;")
	for _, t := range tenants {
		if !t.DNSHijack {
			continue
		}
		s := t.Sets()
		for _, proto := range []string{"udp", "tcp"} {
			w("\t\t# tenant %s", t.Name)
			w("\t\tip saddr @%s %s dport 53 return", s.HostsExc, proto)
			w("\t\tip saddr @%s %s dport 53 redirect to :%d", s.SrcNets, proto, port)
		}
	}
	w("\t}")

	// ---- forward marking ----
	// The mark MUST be set in prerouting (before the routing decision);
	// marking in hook forward would be too late for policy routing.
	w("")
	w("\tchain mangle_pre {")
	w("\t\ttype filter hook prerouting priority -150; policy accept;")
	for _, t := range tenants {
		s := t.Sets()
		w("\t\tip saddr @%s jump %s_fwd", s.SrcNets, setPrefix(t.Index))
		w("\t\tip saddr @%s jump %s_fwd", s.HostsFull, setPrefix(t.Index))
	}
	w("\t}")

	for _, t := range tenants {
		s := t.Sets()
		mark := fmt.Sprintf("0x%x", t.Mark())
		p := setPrefix(t.Index)
		w("")
		w("\tchain %s_fwd {", p)
		w("\t\tip saddr @%s return", s.HostsExc)
		w("\t\tip saddr @%s meta mark set %s return", s.HostsFull, mark)
		w("\t\tip daddr @%s return", s.ExcGeoIP)
		w("\t\tip daddr @%s return", s.ExcStatic)
		w("\t\tip daddr @%s return", s.ExcDNS)
		w("\t\tip daddr @%s meta mark set %s return", s.IncGeoIP, mark)
		w("\t\tip daddr @%s meta mark set %s return", s.IncStatic, mark)
		w("\t\tip daddr @%s meta mark set %s return", s.IncDNS, mark)
		if t.RoutingMode == "invert" {
			w("\t\t# invert: everything else from this tenant goes through the channel")
			w("\t\tmeta mark set %s return", mark)
		}
		w("\t}")
	}
	w("}")
	return b.String()
}

func setPrefix(index int) string { return fmt.Sprintf("t%d", index) }

// renderAddrSet writes a set block; interval=true enables interval+auto-merge
// (required for CIDR elements).
func renderAddrSet(b *strings.Builder, name string, interval bool, elements []string) {
	var w = func(format string, args ...any) { fmt.Fprintf(b, format+"\n", args...) }
	w("\tset %s {", name)
	w("\t\ttype ipv4_addr;")
	if interval {
		w("\t\tflags interval;")
		w("\t\tauto-merge;")
	}
	if len(elements) > 0 {
		w("\t\telements = { %s }", strings.Join(elements, ", "))
	}
	w("\t}")
}

func renderDNSSet(b *strings.Builder, name string, ttlMax int, restore []Elem) {
	var w = func(format string, args ...any) { fmt.Fprintf(b, format+"\n", args...) }
	w("\tset %s {", name)
	w("\t\ttype ipv4_addr;")
	w("\t\tflags timeout;")
	w("\t\ttimeout %ds;", ttlMax)
	var elems []string
	for _, e := range restore {
		if ip := net.ParseIP(e.IP); ip != nil && e.TimeoutSec > 0 {
			elems = append(elems, fmt.Sprintf("%s timeout %ds", ip.To4(), e.TimeoutSec))
		}
	}
	if len(elems) > 0 {
		w("\t\telements = { %s }", strings.Join(elems, ", "))
	}
	w("\t}")
}

// ---- helpers ----

func canonCIDRs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, n, err := net.ParseCIDR(s); err == nil {
			out = append(out, n.String())
		}
	}
	return out
}

func canonIPs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if ip := net.ParseIP(s); ip != nil {
			if v4 := ip.To4(); v4 != nil {
				out = append(out, v4.String())
			}
		}
	}
	return out
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
