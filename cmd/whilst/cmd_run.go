package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/PeterSR/systemd-whilst/systemd"
	"github.com/PeterSR/systemd-whilst/systemd/bus"
	"github.com/PeterSR/systemd-whilst/systemd/ipc"
	"github.com/PeterSR/systemd-whilst/systemd/watch"
	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/engine"
)

func cmdRun(args []string) error {
	fs := newFlags("run")
	path := fs.String("config", systemd.ConfigPath(), "config file")
	debug := fs.Bool("debug", false, "log debug messages")
	noWatch := fs.Bool("no-watch", false, "do not reload when the config file changes")
	if pos, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usagef("run takes no arguments")
	}
	setupLogging(*debug)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hub := bus.NewHub()
	defer hub.Close()
	var lostOnce sync.Once
	var lost error
	hub.OnLost = func(b string, err error) {
		lostOnce.Do(func() { lost = fmt.Errorf("lost the %s bus: %w", b, err) })
		cancel()
	}

	reg := systemd.Registry()
	outputs := systemd.Outputs(hub)
	load := func(allowMissing bool) (*engine.Compiled, error) {
		// Only a missing top-level file means "no config". A missing include
		// is an error, or a typo would silently drop every rule.
		var cfg *config.Config
		if _, err := os.Stat(*path); errors.Is(err, os.ErrNotExist) {
			if !allowMissing {
				return nil, fmt.Errorf("%s was removed; keeping the running config (`whilst reload` runs with no rules)", *path)
			}
			slog.Warn("no config file; running with no rules (temporary rules from `whilst ensure` still work)", "path", *path)
			cfg = config.Empty(*path)
		} else if cfg, err = config.Load(*path, reg); err != nil {
			return nil, err
		}
		return engine.Compile(cfg, outputs)
	}
	// The watched files follow each successful load, so new includes are
	// watched too.
	var watcherPtr atomic.Pointer[watch.Watcher]
	watchFiles := func(files []string) {
		if watcher := watcherPtr.Load(); watcher != nil {
			watch.EnsureDir(*path) // so a config written there later is seen
			abs, _ := filepath.Abs(*path)
			watcher.Set(append([]string{abs}, files...))
		}
	}

	comp, err := load(true)
	if err != nil {
		return err
	}
	for _, w := range comp.Warnings {
		slog.Warn(w)
	}

	e := engine.New(comp, engine.Deps{
		Lifetimes: systemd.Lifetimes{Hub: hub},
		NewSource: systemd.NewSource(hub),
		Outputs:   outputs,
		Actions:   systemd.NewRunner(ctx, hub),
		StatePath: systemd.StatePath(),
		BootID:    systemd.BootID(),
	})
	if err := e.Restore(); err != nil {
		slog.Warn("cannot restore state; starting fresh", "error", err)
	}
	// Catch signals before taking the bus name: with Type=dbus, systemd
	// counts the service as started once the name is taken, and a reload
	// (SIGHUP) right after must not kill the process.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	// reload loads, re-arms the watcher and applies the config, one at a
	// time, so configs are applied in the order they were read. A watcher
	// reload treats a deleted config as an error rather than "no rules".
	var reloadMu sync.Mutex
	watched := comp.Config.Files
	reload := func(why string, allowMissing bool) (*engine.Compiled, error) {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		comp, err := load(allowMissing)
		if err != nil {
			slog.Error("reload failed; keeping the running config", "trigger", why, "error", err)
			// A no-op once the engine stopped.
			e.Do(func() { e.SetConfigError(err) })
			watchFiles(watched) // directories may have been recreated
			return nil, err
		}
		watched = comp.Config.Files
		watchFiles(watched)
		if !e.Do(func() { e.Reload(comp) }) {
			// The engine stopped; the process is shutting down.
			return nil, errors.New("whilst is stopping")
		}
		return comp, nil
	}
	if !*noWatch {
		if w, err := watch.New(ctx, 500*time.Millisecond, func() { reload("file change", false) }); err != nil {
			slog.Warn("cannot watch the config for changes; use `whilst reload`", "error", err)
		} else {
			watcherPtr.Store(w)
			watchFiles(watched)
		}
	}

	if err := ipc.Serve(hub, e, func() (*engine.Compiled, error) { return reload("whilst reload", true) }); err != nil {
		return err
	}

	// After resume, every value may be out of date.
	if _, err := hub.Subscribe(bus.System, bus.Match{
		Type: "signal", Sender: "org.freedesktop.login1", Path: "/org/freedesktop/login1",
		Interface: "org.freedesktop.login1.Manager", Member: "PrepareForSleep",
	}, func(sig *dbus.Signal) {
		if len(sig.Body) == 1 {
			if sleeping, ok := sig.Body[0].(bool); ok && !sleeping {
				slog.Info("resumed from sleep; re-reading every fact")
				e.Resumed()
			}
		}
	}); err != nil {
		slog.Warn("cannot watch for resume", "error", err)
	}

	go func() {
		for s := range sigs {
			if s == syscall.SIGHUP {
				reload("SIGHUP", true)
				continue
			}
			slog.Info("stopping", "signal", s.String())
			cancel()
			return
		}
	}()

	slog.Info("whilst started", "version", version, "config", *path, "facts", len(comp.Facts), "rules", len(comp.Rules))
	if err := e.Run(ctx); err != nil {
		return err
	}
	// Run returns only after ctx is cancelled; OnLost sets lost before
	// cancelling, and lostOnce orders the write before this read.
	lostOnce.Do(func() {})
	return lost
}

func setupLogging(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	// Under systemd the journal adds timestamps already.
	if os.Getenv("INVOCATION_ID") != "" {
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
}
