// Package routing manages per-tenant routing tables, ip rules and reacts to
// interface/route changes via netlink.
package routing

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/leidruid/fencebreaker/daemon/internal/model"
)

// Manager owns the routing-plane state of all tenants.
type Manager struct {
	mu      sync.Mutex
	tenants []*model.Tenant // enabled tenants of the last apply
}

// New creates a manager.
func New() *Manager { return &Manager{} }

// Remember stores the tenant set used by Reconcile.
func (m *Manager) Remember(tenants []*model.Tenant) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tenants = tenants
}

// LinkAddrFunc returns current IPv4 addresses of a link (for netres).
func LinkAddrFunc(dev string) ([]net.Addr, error) {
	link, err := netlink.LinkByName(dev)
	if err != nil {
		return nil, err
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	out := make([]net.Addr, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IPNet)
	}
	return out, nil
}

// Reconcile brings every tenant's table and rules in line with the current
// interface state. It is idempotent and safe to call on every netlink event.
func (m *Manager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	tenants := append([]*model.Tenant(nil), m.tenants...)
	m.mu.Unlock()

	var errs []error
	// snapshot existing rules in our priority range and remove stale ones
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list rules: %w", err)
	}
	wanted := map[int]bool{}
	for _, t := range tenants {
		wanted[t.RulePref()] = true
	}
	for _, r := range rules {
		if r.Priority >= model.RuleBase && r.Priority < model.RuleBase+1000 && !wanted[r.Priority] {
			dr := netlink.NewRule()
			dr.Priority = r.Priority
			dr.Family = netlink.FAMILY_V4
			// RuleList does not fill every field; delete by priority match
			if err := ruleDeleteByPref(r.Priority); err != nil {
				errs = append(errs, err)
			}
		}
	}

	for _, t := range tenants {
		if err := m.reconcileTenant(ctx, t); err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", t.Name, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (m *Manager) reconcileTenant(ctx context.Context, t *model.Tenant) error {
	table := t.TableID()
	// ip rule: fwmark -> tenant table (exact mark value)
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V4
	rule.Table = table
	rule.Priority = t.RulePref()
	rule.Mark = t.Mark()
	mask := uint32(0xffffffff)
	rule.Mask = &mask
	if err := addRuleIdempotent(rule); err != nil {
		return fmt.Errorf("rule pref %d: %w", t.RulePref(), err)
	}

	switch t.EgressMode {
	case model.EgressInterface:
		return reconcileInterfaceEgress(t, table)
	case model.EgressVLESS:
		return reconcileVLESSEgress(t, table)
	}
	return fmt.Errorf("unknown egress mode %q", t.EgressMode)
}

func reconcileInterfaceEgress(t *model.Tenant, table int) error {
	link, err := netlink.LinkByName(t.EgressInterface)
	if err != nil {
		return setEgressState(t, t.Fallback, err)
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil || len(addrs) == 0 {
		return setEgressState(t, t.Fallback, fmt.Errorf("interface %s has no address", t.EgressInterface))
	}
	// default route of that interface in the main table gives us the gateway
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return setEgressState(t, t.Fallback, err)
	}
	var gw net.IP
	var oif int
	found := false
	for _, r := range routes {
		if r.Table != syscall.RT_TABLE_MAIN || r.LinkIndex != link.Attrs().Index {
			continue
		}
		// default route: nil or 0.0.0.0/0 depending on netlink version
		if r.Dst == nil || r.Dst.String() == "0.0.0.0/0" {
			gw = r.Gw
			oif = r.LinkIndex
			found = true
			break
		}
	}
	if !found && p2pLike(link, addrs) {
		// VPN tunnels (WireGuard/AmneziaWG and other point-to-point devices)
		// typically carry no default route in the main table; routing into
		// them is by destination. A device-only default is correct there.
		oif = link.Attrs().Index
		found = true
	}
	if !found {
		return setEgressState(t, t.Fallback, fmt.Errorf("no default route via %s", t.EgressInterface))
	}
	route := &netlink.Route{
		Table:     table,
		LinkIndex: oif,
		Protocol:  syscall.RTPROT_STATIC,
	}
	if gw != nil {
		route.Gw = gw
	} else {
		// device-only default: netlink requires an explicit zero Dst when
		// there is no gateway
		route.Dst = &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
	}
	if err := netlink.RouteReplace(route); err != nil {
		return setEgressState(t, t.Fallback, err)
	}
	return setEgressState(t, "up", nil)
}

// p2pLike reports whether the link behaves like a point-to-point tunnel
// (WireGuard/AmneziaWG and friends): a POINTOPOINT flag in either of the two
// bit layouts (net.Flags vs system IFF_*) or an /32-only address, which is
// how tunnel peers are always addressed.
func p2pLike(link netlink.Link, addrs []netlink.Addr) bool {
	flags := uint32(link.Attrs().Flags)
	if flags&uint32(net.FlagPointToPoint) != 0 || flags&syscall.IFF_POINTOPOINT != 0 {
		return true
	}
	if len(addrs) == 1 {
		ipn := addrs[0].IPNet
		if ipn != nil {
			ones, _ := ipn.Mask.Size()
			return ones == 32
		}
	}
	return false
}

func reconcileVLESSEgress(t *model.Tenant, table int) error {
	link, err := netlink.LinkByName(t.TunName())
	if err != nil {
		return setEgressState(t, t.Fallback, fmt.Errorf("tun %s missing (sing-box down?)", t.TunName()))
	}
	route := &netlink.Route{
		Table:     table,
		LinkIndex: link.Attrs().Index,
		Protocol:  syscall.RTPROT_STATIC,
		Scope:     netlink.SCOPE_LINK,
	}
	if err := netlink.RouteReplace(route); err != nil {
		return setEgressState(t, t.Fallback, err)
	}
	return setEgressState(t, "up", nil)
}

// setEgressState records the channel state; strict mode blocks traffic by
// installing an unreachable default instead of falling back to main.
func setEgressState(t *model.Tenant, state string, cause error) error {
	t.EgressState = state
	t.EgressError = ""
	if cause != nil {
		t.EgressError = cause.Error()
	}
	if state == "up" || t.Fallback != "strict" {
		return cause
	}
	// strict: blackhole the tenant table so traffic does not leak to main
	route := &netlink.Route{
		Table:    t.TableID(),
		Type:     syscall.RTN_UNREACHABLE,
		Protocol: syscall.RTPROT_STATIC,
	}
	if err := netlink.RouteReplace(route); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func addRuleIdempotent(rule *netlink.Rule) error {
	existing, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, r := range existing {
		if r.Priority == rule.Priority && r.Table == rule.Table && r.Mark == rule.Mark {
			return nil
		}
	}
	return netlink.RuleAdd(rule)
}

func ruleDeleteByPref(pref int) error {
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if r.Priority == pref {
			dr := netlink.NewRule()
			dr.Family = netlink.FAMILY_V4
			dr.Priority = pref
			dr.Table = r.Table
			dr.Mark = r.Mark
			if err := netlink.RuleDel(dr); err != nil {
				return err
			}
		}
	}
	return nil
}

// Flush removes all tenant rules and table defaults (called on shutdown or
// when the config disables fencebreaker).
func (m *Manager) Flush(ctx context.Context) error {
	m.mu.Lock()
	tenants := append([]*model.Tenant(nil), m.tenants...)
	m.mu.Unlock()
	var errs []error
	for _, t := range tenants {
		if err := ruleDeleteByPref(t.RulePref()); err != nil {
			errs = append(errs, err)
		}
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: t.TableID()}, netlink.RT_FILTER_TABLE)
		if err == nil {
			for _, r := range routes {
				if err := netlink.RouteDel(&r); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// Watch monitors netlink route/link events and calls cb (debounced) when the
// set of default routes or interfaces changes. It runs until ctx is done.
func Watch(ctx context.Context, cb func()) {
	rch := make(chan netlink.RouteUpdate, 16)
	lch := make(chan netlink.LinkUpdate, 16)
	if err := netlink.RouteSubscribe(rch, ctx.Done()); err != nil {
		return
	}
	if err := netlink.LinkSubscribe(lch, ctx.Done()); err != nil {
		return
	}
	var timer *time.Timer
	debounce := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(500*time.Millisecond, cb)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-rch:
			debounce()
		case <-lch:
			debounce()
		}
	}
}
