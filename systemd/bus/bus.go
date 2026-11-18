// Package bus shares one connection per message bus between everything in
// whilst, and fans incoming signals out to subscribers by match rule.
package bus

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// Bus names accepted in config. "user" is an alias for "session".
const (
	System  = "system"
	Session = "session"
)

// Normalize maps a config bus name to System or Session. Empty means def.
func Normalize(name, def string) (string, error) {
	switch name {
	case "":
		return def, nil
	case "system":
		return System, nil
	case "session", "user":
		return Session, nil
	}
	return "", fmt.Errorf("unknown bus %q (use system, session or user)", name)
}

// CallTimeout bounds every method call whilst makes.
const CallTimeout = 10 * time.Second

// Hub owns the connections.
type Hub struct {
	mu      sync.Mutex
	conns   map[string]*conn
	closing bool
	// OnLost is called once if a connection drops. The daemon exits so that
	// systemd restarts it with fresh connections.
	OnLost func(bus string, err error)
}

type conn struct {
	name   string
	c      *dbus.Conn
	mu     sync.Mutex
	subs   map[int]*sub
	next   int
	owners map[string]string // well-known name -> unique name
	// ownerSubs are notified when a well-known name changes owner.
	ownerSubs map[int]ownerSub
}

type sub struct {
	m  Match
	fn func(*dbus.Signal)
}

type ownerSub struct {
	name string
	fn   func(newOwner string)
}

// NewHub returns a hub that connects lazily.
func NewHub() *Hub { return &Hub{conns: map[string]*conn{}} }

// Conn returns the shared connection to a bus, connecting on first use.
func (h *Hub) Conn(name string) (*dbus.Conn, error) {
	c, err := h.get(name)
	if err != nil {
		return nil, err
	}
	return c.c, nil
}

func (h *Hub) get(name string) (*conn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.conns[name]; ok {
		return c, nil
	}
	var dc *dbus.Conn
	var err error
	switch name {
	// The sequential handler keeps signals in order even when delivery
	// backs up; the default handler may reorder them under load.
	case System:
		dc, err = dbus.ConnectSystemBus(dbus.WithSignalHandler(dbus.NewSequentialSignalHandler()))
	case Session:
		dc, err = dbus.ConnectSessionBus(dbus.WithSignalHandler(dbus.NewSequentialSignalHandler()))
	default:
		return nil, fmt.Errorf("unknown bus %q", name)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to %s bus: %w", name, err)
	}
	c := &conn{name: name, c: dc, subs: map[int]*sub{}, owners: map[string]string{}, ownerSubs: map[int]ownerSub{}}
	ch := make(chan *dbus.Signal, 256)
	dc.Signal(ch)
	if err := dc.AddMatchSignal(
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
	); err != nil {
		dc.Close()
		return nil, fmt.Errorf("watch name owners on %s bus: %w", name, err)
	}
	go c.dispatch(ch, h)
	h.conns[name] = c
	return c, nil
}

func (c *conn) dispatch(ch chan *dbus.Signal, h *Hub) {
	for sig := range ch {
		if sig == nil {
			continue
		}
		if sig.Name == "org.freedesktop.DBus.NameOwnerChanged" && len(sig.Body) == 3 {
			name, _ := sig.Body[0].(string)
			newOwner, _ := sig.Body[2].(string)
			c.mu.Lock()
			_, tracked := c.owners[name]
			if tracked {
				c.owners[name] = newOwner
			}
			var notify []func(string)
			for _, o := range c.ownerSubs {
				if o.name == name {
					notify = append(notify, o.fn)
				}
			}
			// Unique names never come back; forget them so the map does
			// not grow with every attached caller.
			if tracked && newOwner == "" && strings.HasPrefix(name, ":") {
				delete(c.owners, name)
			}
			c.mu.Unlock()
			for _, fn := range notify {
				fn(newOwner)
			}
		}
		c.mu.Lock()
		var fns []func(*dbus.Signal)
		for _, s := range c.subs {
			if s.m.matches(sig, c.owners) {
				fns = append(fns, s.fn)
			}
		}
		c.mu.Unlock()
		for _, fn := range fns {
			fn(sig)
		}
	}
	// The channel is closed when the connection is lost, or by Close.
	h.mu.Lock()
	closing := h.closing
	h.mu.Unlock()
	if closing {
		return
	}
	err := fmt.Errorf("connection closed")
	slog.Error("lost D-Bus connection", "bus", c.name)
	if h.OnLost != nil {
		h.OnLost(c.name, err)
	}
}

// Subscribe adds a match rule on the bus and calls fn for every matching
// signal. fn runs on the dispatch goroutine and must not block.
func (h *Hub) Subscribe(busName string, m Match, fn func(*dbus.Signal)) (cancel func(), err error) {
	c, err := h.get(busName)
	if err != nil {
		return nil, err
	}
	if m.Type == "" {
		m.Type = "signal"
	}
	if m.Sender != "" && !strings.HasPrefix(m.Sender, ":") {
		c.trackOwner(m.Sender)
	}
	rule := m.String()
	if call := busCall(c.c, "org.freedesktop.DBus.AddMatch", rule); call.Err != nil {
		return nil, fmt.Errorf("add match %s: %w", rule, call.Err)
	}
	c.mu.Lock()
	id := c.next
	c.next++
	c.subs[id] = &sub{m: m, fn: fn}
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
		busCall(c.c, "org.freedesktop.DBus.RemoveMatch", rule)
	}, nil
}

// WatchOwner calls fn whenever the well-known name gets a new owner or loses
// its owner (newOwner is then empty).
func (h *Hub) WatchOwner(busName, name string, fn func(newOwner string)) (cancel func(), err error) {
	c, err := h.get(busName)
	if err != nil {
		return nil, err
	}
	c.trackOwner(name)
	c.mu.Lock()
	id := c.next
	c.next++
	c.ownerSubs[id] = ownerSub{name: name, fn: fn}
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.ownerSubs, id)
		// Forget a unique name nobody watches any more.
		if strings.HasPrefix(name, ":") {
			watched := false
			for _, o := range c.ownerSubs {
				watched = watched || o.name == name
			}
			if !watched {
				delete(c.owners, name)
			}
		}
		c.mu.Unlock()
	}, nil
}

// unknownOwner marks a name whose owner is being looked up.
const unknownOwner = "\x00?"

func (c *conn) trackOwner(name string) {
	c.mu.Lock()
	if _, ok := c.owners[name]; ok {
		c.mu.Unlock()
		return
	}
	// Track first, then ask: a NameOwnerChanged that arrives during the
	// call updates the entry, and the (older) answer is then dropped.
	c.owners[name] = unknownOwner
	c.mu.Unlock()
	var owner string
	_ = busCall(c.c, "org.freedesktop.DBus.GetNameOwner", name).Store(&owner)
	c.mu.Lock()
	if c.owners[name] == unknownOwner {
		c.owners[name] = owner
	}
	c.mu.Unlock()
}

// Close closes every connection.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closing = true
	for _, c := range h.conns {
		c.c.Close()
	}
	h.conns = map[string]*conn{}
}

// busCall calls the message bus itself with the standard timeout.
func busCall(c *dbus.Conn, method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
	defer cancel()
	return c.BusObject().CallWithContext(ctx, method, 0, args...)
}

// NameHasOwner reports whether a name currently has an owner on the bus.
func (h *Hub) NameHasOwner(busName, name string) bool {
	c, err := h.get(busName)
	if err != nil {
		return false
	}
	var has bool
	if err := busCall(c.c, "org.freedesktop.DBus.NameHasOwner", name).Store(&has); err != nil {
		return false
	}
	return has
}

// Call makes a method call with the standard timeout.
func Call(c *dbus.Conn, dest, path, method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
	defer cancel()
	return c.Object(dest, dbus.ObjectPath(path)).CallWithContext(ctx, method, 0, args...)
}

// GetProperty reads one property with the standard timeout.
func GetProperty(c *dbus.Conn, dest, path, iface, prop string) (dbus.Variant, error) {
	var v dbus.Variant
	err := Call(c, dest, path, "org.freedesktop.DBus.Properties.Get", iface, prop).Store(&v)
	return v, err
}
