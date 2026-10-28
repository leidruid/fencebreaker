package fbtenant

import (
	"context"
	"errors"
	"os"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/log"
)

// init registers the plugin with the CoreDNS server.
func init() {
	plugin.Register("fbtenant", setup)
}

const pluginName = "fbtenant"

// setup parses the Corefile block:
//
//	fbtenant {
//	    state FILE
//	}
//
// and installs the plugin. A background goroutine (started with the server)
// watches dns_state.json, resyncs mirrors and writes plugin_status.json.
func setup(c *caddy.Controller) error {
	f := New()
	found := false
	for c.Next() {
		found = true
		for c.NextBlock() {
			switch c.Val() {
			case "state":
				if !c.NextArg() {
					return plugin.Error(pluginName, c.ArgErr())
				}
				f.stateFile = c.Val()
			default:
				return plugin.Error(pluginName, c.Errf("unknown property %q", c.Val()))
			}
		}
	}
	if !found {
		return plugin.Error(pluginName, errors.New("fbtenant block is required"))
	}
	if f.stateFile == "" {
		return plugin.Error(pluginName, errors.New(`"state FILE" is required in the fbtenant block`))
	}
	f.statusFile = statusPathFor(f.stateFile)
	f.keeperFile = keeperPathFor(f.stateFile)

	// Best-effort initial load: the daemon may not have written the state
	// file yet; the watcher picks it up as soon as it appears.
	if err := f.initialLoad(); err != nil {
		return plugin.Error(pluginName, err)
	}

	c.OnStartup(func() error {
		ctx := context.Background()
		f.startWatcher(ctx)
		log.Infof("fbtenant: watching %s (status: %s)", f.stateFile, f.statusFile)
		return nil
	})
	c.OnShutdown(func() error {
		f.Stop()
		return nil
	})

	// Hook the handler into the server's plugin chain; without this the
	// server never calls ServeDNS (queries fall through with "no next plugin
	// found").
	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		f.Next = next
		return f
	})

	return nil
}

// initialLoad performs the first state load and mirror resync.
func (f *FBTenant) initialLoad() error {
	if _, err := loadStateFile(f.stateFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Warningf("fbtenant: state file %s not present yet; waiting for the daemon", f.stateFile)
			return nil
		}
		return err
	}
	return f.reloadState(nil)
}
