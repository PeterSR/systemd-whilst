// Package ipc exposes the running engine on the session bus, and is the
// client the CLI uses to talk to it.
package ipc

import (
	"encoding/json"
	"fmt"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"

	"github.com/PeterSR/systemd-whilst/internal/bus"
	"github.com/PeterSR/systemd-whilst/internal/engine"
)

// Bus name, object path and interface of the running engine.
const (
	Name  = "io.github.PeterSR.Whilst"
	Path  = "/io/github/PeterSR/Whilst"
	Iface = "io.github.PeterSR.Whilst1"
)

// Reloader loads, compiles and applies the config again, recording a
// failure in the engine; it runs off the engine goroutine because it reads
// files.
type Reloader func() (*engine.Compiled, error)

// Server is the exported object.
type Server struct {
	e      *engine.Engine
	reload Reloader
}

const introspection = `
<node>
  <interface name="` + Iface + `">
    <method name="Status"><arg direction="out" type="s"/></method>
    <method name="Ensure"><arg direction="in" type="s"/><arg direction="out" type="s"/></method>
    <method name="Release"><arg direction="in" type="s"/><arg direction="out" type="i"/></method>
    <method name="Reload"><arg direction="out" type="s"/></method>
  </interface>` + introspect.IntrospectDataString + `</node>`

// Serve exports the engine on the session bus and claims the bus name. It
// fails if another whilst already runs.
func Serve(hub *bus.Hub, e *engine.Engine, reload Reloader) error {
	c, err := hub.Conn(bus.Session)
	if err != nil {
		return err
	}
	s := &Server{e: e, reload: reload}
	if err := c.Export(s, Path, Iface); err != nil {
		return err
	}
	if err := c.Export(introspect.Introspectable(introspection), Path, "org.freedesktop.DBus.Introspectable"); err != nil {
		return err
	}
	reply, err := c.RequestName(Name, dbus.NameFlagDoNotQueue)
	if err != nil {
		return err
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("another whilst is already running (%s is taken)", Name)
	}
	return nil
}

func dbusErr(err error) *dbus.Error {
	return dbus.NewError(Iface+".Error", []any{err.Error()})
}

// Status returns the engine snapshot as JSON.
func (s *Server) Status() (string, *dbus.Error) {
	var st engine.Status
	s.e.Do(func() { st = s.e.Snapshot() })
	b, err := json.Marshal(st)
	if err != nil {
		return "", dbusErr(err)
	}
	return string(b), nil
}

// Ensure adds a temporary rule described by a JSON TempSpec.
func (s *Server) Ensure(sender dbus.Sender, specJSON string) (string, *dbus.Error) {
	var spec engine.TempSpec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil {
		return "", dbusErr(fmt.Errorf("bad request: %w", err))
	}
	var res EnsureResult
	var err error
	s.e.Do(func() { res.ID, res.Problems, res.Waiting, err = s.e.EnsureNow(spec, string(sender)) })
	if err != nil {
		return "", dbusErr(err)
	}
	b, _ := json.Marshal(res)
	return string(b), nil
}

// EnsureResult is the reply to Ensure.
type EnsureResult struct {
	ID string `json:"id"`
	// Problems lists outputs that could not be applied yet.
	Problems []string `json:"problems,omitempty"`
	// Waiting says why the condition cannot be evaluated yet, if so.
	Waiting string `json:"waiting,omitempty"`
}

// Release drops temporary rules by id or name, or all with "".
func (s *Server) Release(id string) (int32, *dbus.Error) {
	var n int
	s.e.Do(func() { n = s.e.Release(id) })
	return int32(n), nil
}

// Reload re-reads the config.
func (s *Server) Reload() (string, *dbus.Error) {
	comp, err := s.reload()
	if err != nil {
		return "", dbusErr(err)
	}
	return fmt.Sprintf("loaded %d facts and %d rules", len(comp.Facts), len(comp.Rules)), nil
}

// Client calls a running whilst.
type Client struct {
	obj dbus.BusObject
	c   *dbus.Conn
}

// Dial connects to the session bus. The connection is private so that an
// attached temporary rule lives exactly as long as this process.
func Dial() (*Client, error) {
	c, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("connect to session bus: %w", err)
	}
	return &Client{c: c, obj: c.Object(Name, Path)}, nil
}

func (cl *Client) call(method string, args ...any) *dbus.Call {
	call := cl.obj.Call(Iface+"."+method, 0, args...)
	if e, ok := call.Err.(dbus.Error); ok {
		switch e.Name {
		case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner":
			call.Err = fmt.Errorf("whilst is not running (start it with `systemctl --user start whilst` or `whilst run`)")
		case Iface + ".Error":
			if len(e.Body) > 0 {
				call.Err = fmt.Errorf("%v", e.Body[0])
			}
		}
	}
	return call
}

// Status fetches the engine snapshot.
func (cl *Client) Status() (*engine.Status, error) {
	var raw string
	if err := cl.call("Status").Store(&raw); err != nil {
		return nil, err
	}
	var st engine.Status
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Ensure adds a temporary rule. The engine applies it before replying.
func (cl *Client) Ensure(spec engine.TempSpec) (EnsureResult, error) {
	var res EnsureResult
	b, err := json.Marshal(spec)
	if err != nil {
		return res, err
	}
	var raw string
	if err := cl.call("Ensure", string(b)).Store(&raw); err != nil {
		return res, err
	}
	err = json.Unmarshal([]byte(raw), &res)
	return res, err
}

// WatchRestarts calls fn(true) whenever whilst (re)appears on the bus and
// fn(false) when it goes away, until the client is closed.
func (cl *Client) WatchRestarts(fn func(up bool)) error {
	if err := cl.c.AddMatchSignal(
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, Name),
	); err != nil {
		return err
	}
	ch := make(chan *dbus.Signal, 16)
	cl.c.Signal(ch)
	go func() {
		for sig := range ch {
			if sig.Name != "org.freedesktop.DBus.NameOwnerChanged" || len(sig.Body) != 3 {
				continue
			}
			if name, _ := sig.Body[0].(string); name != Name {
				continue
			}
			newOwner, _ := sig.Body[2].(string)
			fn(newOwner != "")
		}
	}()
	return nil
}

// Release drops temporary rules.
func (cl *Client) Release(id string) (int, error) {
	var n int32
	err := cl.call("Release", id).Store(&n)
	return int(n), err
}

// Reload asks for a config reload.
func (cl *Client) Reload() (string, error) {
	var msg string
	err := cl.call("Reload").Store(&msg)
	return msg, err
}

// Close closes the connection.
func (cl *Client) Close() { cl.c.Close() }
