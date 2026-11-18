package source

import (
	"context"
	"fmt"
	"sync"

	"github.com/godbus/dbus/v5"

	"github.com/PeterSR/systemd-whilst/systemd/bus"
)

// dbusProp reads a property and follows PropertiesChanged. When the owning
// service restarts, the value is re-read.
type dbusProp struct {
	hub *bus.Hub
	cfg DBusProp
}

func (d *dbusProp) describe() string {
	return fmt.Sprintf("D-Bus %s %s %s.%s", d.cfg.Bus, d.cfg.Path, d.cfg.Interface, d.cfg.Property)
}

func (d *dbusProp) read(ctx context.Context) (any, error) {
	c, err := d.hub.Conn(d.cfg.Bus)
	if err != nil {
		return nil, err
	}
	v, err := bus.GetProperty(c, d.cfg.Service, d.cfg.Path, d.cfg.Interface, d.cfg.Property)
	if err != nil {
		return nil, err
	}
	return v.Value(), nil
}

func (d *dbusProp) watch(ctx context.Context, push func(any), refresh func()) error {
	return watchProperty(ctx, d.hub, d.cfg.Bus, d.cfg.Service, d.cfg.Path, d.cfg.Interface, d.cfg.Property, push, refresh)
}

func watchProperty(ctx context.Context, hub *bus.Hub, busName, service, path, iface, prop string, push func(any), refresh func()) error {
	m := bus.Match{
		Type:      "signal",
		Sender:    service,
		Path:      path,
		Interface: "org.freedesktop.DBus.Properties",
		Member:    "PropertiesChanged",
		Args:      map[int]string{0: iface},
	}
	cancel, err := hub.Subscribe(busName, m, func(sig *dbus.Signal) {
		if len(sig.Body) < 3 {
			return
		}
		if changed, ok := sig.Body[1].(map[string]dbus.Variant); ok {
			if v, ok := changed[prop]; ok {
				push(v.Value())
				return
			}
		}
		if inval, ok := sig.Body[2].([]string); ok {
			for _, p := range inval {
				if p == prop {
					refresh()
					return
				}
			}
		}
	})
	if err != nil {
		return err
	}
	logCancel(ctx, path, cancel)
	cancelOwner, err := hub.WatchOwner(busName, service, func(newOwner string) {
		if newOwner != "" {
			refresh()
		}
	})
	if err != nil {
		return err
	}
	logCancel(ctx, service, cancelOwner)
	return nil
}

// unitState reads a systemd unit's ActiveState. systemd only sends unit
// property changes to clients that called Subscribe on the manager.
type unitState struct {
	hub *bus.Hub
	cfg Unit

	mu   sync.Mutex
	path dbus.ObjectPath
}

const systemd = "org.freedesktop.systemd1"

func (u *unitState) describe() string {
	return fmt.Sprintf("systemd unit %s (%s)", u.cfg.Name, u.cfg.Bus)
}

func (u *unitState) unitPath() (dbus.ObjectPath, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.path != "" {
		return u.path, nil
	}
	c, err := u.hub.Conn(u.cfg.Bus)
	if err != nil {
		return "", err
	}
	var p dbus.ObjectPath
	// LoadUnit works for units that are not loaded yet; GetUnit would fail.
	if err := bus.Call(c, systemd, "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager.LoadUnit", u.cfg.Name).Store(&p); err != nil {
		return "", err
	}
	u.path = p
	return p, nil
}

func (u *unitState) read(ctx context.Context) (any, error) {
	p, err := u.unitPath()
	if err != nil {
		return nil, err
	}
	c, err := u.hub.Conn(u.cfg.Bus)
	if err != nil {
		return nil, err
	}
	v, err := bus.GetProperty(c, systemd, string(p), "org.freedesktop.systemd1.Unit", "ActiveState")
	if err != nil {
		return nil, err
	}
	return v.Value(), nil
}

func (u *unitState) watch(ctx context.Context, push func(any), refresh func()) error {
	c, err := u.hub.Conn(u.cfg.Bus)
	if err != nil {
		return err
	}
	if err := subscribeSystemd(c); err != nil {
		return err
	}
	p, err := u.unitPath()
	if err != nil {
		return err
	}
	return watchProperty(ctx, u.hub, u.cfg.Bus, systemd, string(p), "org.freedesktop.systemd1.Unit", "ActiveState", push, refresh)
}

var subscribed sync.Map // *dbus.Conn -> bool

func subscribeSystemd(c *dbus.Conn) error {
	if _, ok := subscribed.Load(c); ok {
		return nil
	}
	if call := bus.Call(c, systemd, "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager.Subscribe"); call.Err != nil {
		// Subscribing twice from one connection is an error we can ignore.
		if e, ok := call.Err.(dbus.Error); !ok || e.Name != "org.freedesktop.systemd1.AlreadySubscribed" {
			return fmt.Errorf("subscribe to systemd: %w", call.Err)
		}
	}
	subscribed.Store(c, true)
	return nil
}

// dbusSignal takes its value from signals. It cannot be re-read.
type dbusSignal struct {
	hub  *bus.Hub
	fact string
	cfg  DBusSignal
}

func (s *dbusSignal) Describe() string {
	return fmt.Sprintf("D-Bus signal on %s bus: %s", s.cfg.Bus, s.cfg.Match.String())
}

func (s *dbusSignal) Refresh() {}

func (s *dbusSignal) Start(ctx context.Context, emit func(Update)) error {
	return startErr(s.start(ctx, emit))
}

func (s *dbusSignal) start(ctx context.Context, emit func(Update)) error {
	cancel, err := s.hub.Subscribe(s.cfg.Bus, s.cfg.Match, func(sig *dbus.Signal) {
		if ctx.Err() != nil {
			return
		}
		var v any
		switch {
		case s.cfg.Arg >= 0:
			if s.cfg.Arg >= len(sig.Body) {
				emit(Update{Fact: s.fact, Err: fmt.Errorf("signal has no argument %d", s.cfg.Arg)})
				return
			}
			v = sig.Body[s.cfg.Arg]
		case len(sig.Body) == 1:
			v = sig.Body[0]
		default:
			v = sig.Body
		}
		emit(Update{Fact: s.fact, Value: bus.Value(v)})
	})
	if err != nil {
		return err
	}
	logCancel(ctx, s.cfg.Match.String(), cancel)
	return nil
}
