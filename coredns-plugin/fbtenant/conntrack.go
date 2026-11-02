package fbtenant

import (
	"os/exec"
	"sync"

	"github.com/coredns/coredns/plugin/pkg/log"
)

// ConntrackFlusher drops conntrack entries towards given destination IPs.
// Policy routing applies to new flows only; established connections keep
// their original route. Flushing the conntrack entries of a freshly learned
// IP makes existing client connections re-route immediately (they reconnect
// through the tenant channel instead of waiting for the browser to recycle
// the socket).
type ConntrackFlusher interface {
	FlushDst(ips []string)
}

// execConntrack runs the conntrack binary, one delete per IP.
type execConntrack struct{}

// maxFlushIPs caps the per-batch exec count; a single DNS answer carries at
// most a handful of A records, this guard is for pathological inputs.
const maxFlushIPs = 32

var (
	conntrackMissingOnce sync.Once
)

// FlushDst implements ConntrackFlusher. Best effort: a missing binary is
// logged once (the daemon keeps working, routing switches on new flows
// only), other failures are logged per call.
func (execConntrack) FlushDst(ips []string) {
	if len(ips) > maxFlushIPs {
		ips = ips[:maxFlushIPs]
	}
	for _, ip := range ips {
		cmd := exec.Command("conntrack", "-D", "-d", ip)
		if err := cmd.Run(); err != nil {
			if _, lookErr := exec.LookPath("conntrack"); lookErr != nil {
				conntrackMissingOnce.Do(func() {
					log.Warningf("fbtenant: conntrack not installed; learned IPs will re-route on new flows only (install the conntrack package)")
				})
				return
			}
			log.Debugf("fbtenant: conntrack flush %s: %v", ip, err)
		}
	}
}
