package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/leidruid/fencebreaker/daemon/internal/apply"
	"github.com/leidruid/fencebreaker/daemon/internal/routing"
)

// Version is the daemon version string.
const Version = "0.1.0"

func main() { os.Exit(run()) }

func run() int {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("fencebreakerd: ")

	var (
		configPath  = flag.String("config", "/etc/config/fencebreaker", "UCI config path")
		networkPath = flag.String("network", "/etc/config/network", "UCI network config path")
		runDir      = flag.String("rundir", "/var/run/fencebreaker", "runtime state directory")
		listsDir    = flag.String("listsdir", "/etc/fencebreaker/lists", "list cache directory")
		geodataPath = flag.String("geodata", "/etc/fencebreaker/geoip.dat", "geoip.dat path")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println("fencebreakerd", Version)
		return 0
	}

	paths := apply.Paths{
		Config:   *configPath,
		Network:  *networkPath,
		RunDir:   *runDir,
		ListsDir: *listsDir,
		Geodata:  *geodataPath,
	}
	if err := os.MkdirAll(paths.RunDir, 0o755); err != nil {
		log.Printf("mkdir %s: %v", paths.RunDir, err)
		return 1
	}

	orch := apply.New(paths, apply.ExecRunner{}, log.Default())
	orch.Started = time.Now()
	if err := orch.LoadApplied(); err != nil {
		log.Printf("no previous apply state (%v)", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	if err := orch.Full(ctx); err != nil {
		log.Printf("initial apply failed: %v", err)
	}

	// netlink watcher: reconcile routes on link/route changes
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		routing.Watch(ctx, func() {
			if err := orch.RoutesOnly(context.Background()); err != nil {
				log.Printf("routes reconcile failed: %v", err)
			}
		})
	}()

	statusTick := time.NewTicker(10 * time.Second)
	defer statusTick.Stop()
	refreshTick := time.NewTicker(time.Minute)
	defer refreshTick.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("shutting down")
			return 0
		case <-hup:
			log.Printf("SIGHUP: re-applying configuration")
			if err := orch.Full(context.Background()); err != nil {
				log.Printf("apply failed: %v", err)
			}
		case <-statusTick.C:
			orch.WriteStatusNow()
		case <-refreshTick.C:
			// refresh due lists; full re-apply only when something changed
			if changed := orch.RefreshDue(context.Background()); changed {
				if err := orch.Full(context.Background()); err != nil {
					log.Printf("apply after list refresh failed: %v", err)
				}
			}
		}
	}
}
