package ipc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/engine"
)

// newServer runs an engine with no config and no outputs, and returns a
// Server for it (not exported on any bus) and a function that stops it.
func newServer(t *testing.T) (*Server, func()) {
	t.Helper()
	comp, err := engine.Compile(config.Empty("(test)"), nil)
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(comp, engine.Deps{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Run(ctx) }()
	return &Server{e: e}, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
		}
	}
}

// errText returns the message of an error from the Server, or a
// description of what came back instead.
func errText(err *dbus.Error) string {
	if err == nil {
		return "(no error)"
	}
	if err.Name != Iface+".Error" || len(err.Body) != 1 {
		return fmt.Sprintf("unexpected error %#v", err)
	}
	return fmt.Sprint(err.Body[0])
}

// The engine reports these neutrally; the D-Bus API keeps the wording of
// v0.4.0.
func TestEnsureErrorWording(t *testing.T) {
	s, stop := newServer(t)
	defer stop()
	for _, c := range []struct{ spec, want string }{
		{`{"outputs":{}}`, "name at least one output, like inhibit=sleep"},
		{`{"outputs":null}`, "name at least one output, like inhibit=sleep"},
	} {
		_, err := s.Ensure("", c.spec)
		if got := errText(err); got != c.want {
			t.Errorf("Ensure(%s): got %q, want %q", c.spec, got, c.want)
		}
	}
}

// Calls that arrive while whilst shuts down, after the engine stopped,
// fail at once instead of hanging until the caller times out.
func TestCallsAfterStop(t *testing.T) {
	s, stop := newServer(t)
	stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := s.Status()
		if got := errText(err); got != "whilst is stopping" {
			t.Errorf("Status: %q", got)
		}
		_, err = s.Ensure("", `{"outputs":{"inhibit":"sleep"}}`)
		if got := errText(err); got != "whilst is stopping" {
			t.Errorf("Ensure: %q", got)
		}
		_, err = s.Release("")
		if got := errText(err); got != "whilst is stopping" {
			t.Errorf("Release: %q", got)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a call blocked after the engine stopped")
	}
}
