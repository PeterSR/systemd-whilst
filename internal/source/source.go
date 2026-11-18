// Package source reads fact values from D-Bus, systemd, commands and files,
// and re-reads them when their triggers fire.
package source

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/PeterSR/systemd-whilst/internal/bus"
	"github.com/PeterSR/systemd-whilst/internal/config"
	"github.com/PeterSR/systemd-whilst/internal/value"
)

// Update carries a new raw value for a fact, or the error from reading it.
type Update struct {
	Fact  string
	Value any
	Err   error
	// Seq orders updates from one source: the engine drops an update with a
	// lower Seq than one it already applied. Zero means unordered.
	Seq int64
}

// Source produces updates for one fact.
type Source interface {
	// Start begins watching. It must not block; the first read happens in
	// the background and is reported through emit.
	Start(ctx context.Context, emit func(Update)) error
	// Refresh re-reads the value in the background, if the source can.
	Refresh()
	// Describe is a short human description, shown in status.
	Describe() string
}

// New builds the source for a fact. expr and state facts have no source.
func New(f *config.Fact, hub *bus.Hub) (Source, error) {
	var inner reader
	switch f.Kind {
	case config.SrcDBus:
		inner = &dbusProp{hub: hub, cfg: *f.DBus}
	case config.SrcUnit:
		inner = &unitState{hub: hub, cfg: *f.Unit}
	case config.SrcCommand:
		inner = &command{cfg: *f.Command}
	case config.SrcFile:
		inner = &file{path: f.File}
	case config.SrcDBusSignal:
		return &dbusSignal{hub: hub, fact: f.Name, cfg: *f.DBusSignal}, nil
	default:
		return nil, nil
	}
	return &triggered{fact: f, hub: hub, r: inner}, nil
}

// reader is a source that can be read on demand and may also push values.
type reader interface {
	// read returns the current value.
	read(ctx context.Context) (any, error)
	// watch subscribes to pushed changes; push delivers a value directly,
	// refresh asks for a re-read. It may be a no-op.
	watch(ctx context.Context, push func(any), refresh func()) error
	describe() string
}

// triggered wraps a reader with the generic triggers (on_dbus, every) and
// coalesces overlapping refreshes: a refresh requested while a read is in
// flight causes exactly one more read afterwards.
type triggered struct {
	fact *config.Fact
	hub  *bus.Hub
	r    reader

	mu      sync.Mutex
	ctx     context.Context
	emit    func(Update)
	running bool
	again   bool
	// pushes counts values pushed by signals. A read that overlaps a push
	// may have fetched the older value, so its result is discarded and the
	// read repeated. It also orders updates (Update.Seq): a read is tagged
	// with the count when it started, so if it is posted after a newer push
	// anyway, the engine drops it.
	pushes int64
	// gen counts Start calls. A read goroutine from an earlier (cancelled)
	// attempt must not touch running/again, which belong to the newer one.
	gen int
}

func (t *triggered) Describe() string {
	d := t.r.describe()
	if t.fact.Every > 0 {
		d += fmt.Sprintf(", every %s", t.fact.Every)
	}
	if n := len(t.fact.OnDBus) + len(t.fact.OnDBusSession); n > 0 {
		d += fmt.Sprintf(", on %d D-Bus match(es)", n)
	}
	return d
}

func (t *triggered) Start(ctx context.Context, emit func(Update)) error {
	t.mu.Lock()
	t.ctx, t.emit = ctx, emit
	t.gen++
	t.running, t.again = false, false
	t.mu.Unlock()
	push := func(v any) {
		if ctx.Err() != nil {
			return
		}
		t.mu.Lock()
		t.pushes++
		seq := t.pushes + 1 // reads started before this push carry <= pushes
		t.mu.Unlock()
		emit(Update{Fact: t.fact.Name, Value: value.Normalize(v), Seq: seq})
	}
	if err := t.r.watch(ctx, push, t.Refresh); err != nil {
		return err
	}
	for busName, matches := range map[string][]bus.Match{bus.System: t.fact.OnDBus, bus.Session: t.fact.OnDBusSession} {
		for _, m := range matches {
			cancel, err := t.hub.Subscribe(busName, m, func(*dbus.Signal) { t.Refresh() })
			if err != nil {
				return err
			}
			go func() { <-ctx.Done(); cancel() }()
		}
	}
	if t.fact.Every > 0 {
		go func() {
			tick := time.NewTicker(t.fact.Every)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					t.Refresh()
				}
			}
		}()
	}
	t.Refresh()
	return nil
}

func (t *triggered) Refresh() {
	t.mu.Lock()
	if t.ctx == nil || t.ctx.Err() != nil {
		t.mu.Unlock()
		return
	}
	if t.running {
		t.again = true
		t.mu.Unlock()
		return
	}
	t.running = true
	ctx, emit, gen := t.ctx, t.emit, t.gen
	t.mu.Unlock()
	go func() {
		for {
			t.mu.Lock()
			before := t.pushes
			t.mu.Unlock()
			v, err := t.r.read(ctx)
			if ctx.Err() != nil {
				// Cancelled: clear the flags so a later Start (a retry)
				// can read again, unless that Start already took over.
				t.mu.Lock()
				if t.gen == gen {
					t.running, t.again = false, false
				}
				t.mu.Unlock()
				return
			}
			t.mu.Lock()
			overtaken := t.pushes != before
			t.mu.Unlock()
			if overtaken {
				// A newer value arrived by signal while reading; this
				// result may be older. Read again rather than emit it.
				continue
			}
			if err != nil {
				emit(Update{Fact: t.fact.Name, Err: err, Seq: before + 1})
			} else {
				emit(Update{Fact: t.fact.Name, Value: value.Normalize(v), Seq: before + 1})
			}
			t.mu.Lock()
			if t.gen != gen {
				// A newer Start owns the flags now.
				t.mu.Unlock()
				return
			}
			if !t.again {
				t.running = false
				t.mu.Unlock()
				return
			}
			t.again = false
			t.mu.Unlock()
		}
	}()
}

func logCancel(ctx context.Context, what string, cancel func()) {
	go func() {
		<-ctx.Done()
		cancel()
		slog.Debug("stopped watching", "what", what)
	}()
}
