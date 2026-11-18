package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/godbus/dbus/v5"

	"github.com/PeterSR/systemd-whilst/internal/action"
	"github.com/PeterSR/systemd-whilst/internal/bus"
	"github.com/PeterSR/systemd-whilst/internal/config"
	"github.com/PeterSR/systemd-whilst/internal/engine"
	"github.com/PeterSR/systemd-whilst/internal/ipc"
	"github.com/PeterSR/systemd-whilst/internal/output"
	"github.com/PeterSR/systemd-whilst/internal/source"
)

func cmdRun(args []string) error {
	fs := newFlags("run")
	path := fs.String("config", config.DefaultPath(), "config file")
	debug := fs.Bool("debug", false, "log debug messages")
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

	outputs := []output.Output{
		output.NewInhibit(output.LogindLocker{Hub: hub}),
		output.NewUnits(output.UserSystemd{Hub: hub}),
	}
	load := func() (*engine.Compiled, error) {
		// Only a missing top-level file means "no config". A missing include
		// is an error, or a typo would silently drop every rule.
		var cfg *config.Config
		if _, err := os.Stat(*path); errors.Is(err, os.ErrNotExist) {
			slog.Warn("no config file; running with no rules (temporary rules from `whilst ensure` still work)", "path", *path)
			cfg = config.Empty(*path)
		} else if cfg, err = config.Load(*path); err != nil {
			return nil, err
		}
		return engine.Compile(cfg, outputs)
	}
	comp, err := load()
	if err != nil {
		return err
	}
	for _, w := range comp.Warnings {
		slog.Warn(w)
	}

	e := engine.New(comp, engine.Deps{
		Hub:       hub,
		NewSource: func(f *config.Fact) (source.Source, error) { return source.New(f, hub) },
		Outputs:   outputs,
		Actions:   action.NewBusRunner(ctx, hub),
		StatePath: engine.DefaultStatePath(),
		BootID:    engine.ReadBootID(),
	})
	if err := e.Restore(); err != nil {
		slog.Warn("cannot restore state; starting fresh", "error", err)
	}
	// Catch signals before taking the bus name: with Type=dbus, systemd
	// counts the service as started once the name is taken, and a reload
	// (SIGHUP) right after must not kill the process.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	if err := ipc.Serve(hub, e, load); err != nil {
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
				comp, err := load()
				if err != nil {
					slog.Error("reload failed; keeping the running config", "error", err)
					e.Do(func() { e.SetConfigError(err) })
					continue
				}
				e.Do(func() { e.Reload(comp) })
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
