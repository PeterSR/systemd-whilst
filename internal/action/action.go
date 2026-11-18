// Package action runs the one-off enter and exit actions of rules:
// notifications, commands and D-Bus calls. (set actions change facts and are
// handled by the engine itself.)
package action

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/godbus/dbus/v5"

	"github.com/PeterSR/systemd-whilst/internal/bus"
	"github.com/PeterSR/systemd-whilst/internal/config"
	"github.com/PeterSR/systemd-whilst/internal/source"
)

// Runner runs actions. Run must not block the caller.
type Runner interface {
	// Run queues one rule's enter or exit actions. Actions run in the order
	// they are listed, and batches run in the order they were queued, so a
	// rule's exit actions finish before the next rule's enter actions start.
	Run(rule, event string, actions []config.Action)
}

type batch struct {
	rule, event string
	actions     []config.Action
}

// BusRunner runs actions for real, one at a time, on a single worker.
type BusRunner struct {
	hub   *bus.Hub
	ctx   context.Context
	queue chan batch
}

// NewBusRunner starts the worker; it stops when ctx is cancelled.
func NewBusRunner(ctx context.Context, hub *bus.Hub) *BusRunner {
	r := &BusRunner{hub: hub, ctx: ctx, queue: make(chan batch, 256)}
	go r.work()
	return r
}

// Run queues the actions. If the queue is full (actions are stuck), the batch
// is dropped with an error rather than stalling the engine.
func (r *BusRunner) Run(rule, event string, actions []config.Action) {
	select {
	case r.queue <- batch{rule, event, actions}:
	default:
		slog.Error("action queue is full; dropping actions", "rule", rule, "event", event)
	}
}

func (r *BusRunner) work() {
	for {
		select {
		case <-r.ctx.Done():
			return
		case b := <-r.queue:
			for _, a := range b.actions {
				log := slog.With("rule", b.rule, "event", b.event, "action", a.Kind())
				if err := r.run(b.rule, b.event, a); err != nil {
					log.Warn("action failed", "error", err)
				} else {
					log.Info("action done")
				}
			}
		}
	}
}

func (r *BusRunner) run(rule, event string, a config.Action) error {
	switch {
	case a.Notify != nil:
		return r.notify(a.Notify)
	case a.Command != nil:
		env := []string{"WHILST_RULE=" + rule, "WHILST_EVENT=" + event}
		// Output goes to the journal; a command may start background jobs.
		_, _, code, err := source.RunShell(r.ctx, a.Command.Run, a.Command.Timeout, env, false)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("exited with status %d (its output is in the log above)", code)
		}
		return nil
	case a.DBusCall != nil:
		d := a.DBusCall
		c, err := r.hub.Conn(d.Bus)
		if err != nil {
			return err
		}
		args, err := config.ConvertArgs(d.Signature, d.Args)
		if err != nil {
			return err
		}
		return bus.Call(c, d.Service, d.Path, d.Interface+"."+d.Method, args...).Err
	}
	return fmt.Errorf("unsupported action")
}

func (r *BusRunner) notify(n *config.Notify) error {
	c, err := r.hub.Conn(bus.Session)
	if err != nil {
		return err
	}
	urgency := map[string]byte{"low": 0, "normal": 1, "critical": 2}[n.Urgency]
	hints := map[string]dbus.Variant{"urgency": dbus.MakeVariant(urgency)}
	return bus.Call(c, "org.freedesktop.Notifications", "/org/freedesktop/Notifications",
		"org.freedesktop.Notifications.Notify",
		"whilst", uint32(0), "", n.Summary, n.Body, []string{}, hints, int32(-1)).Err
}
