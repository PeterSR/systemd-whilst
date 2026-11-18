// Package systemd is whilst's Linux application around the whilst engine. It
// registers the fact kinds (dbus, dbus_signal, unit, command, file), the
// action kinds (notify, command, dbus_call) and the presets with the engine's
// config, and builds the sources, outputs, action runner and lifetimes the
// engine runs with.
package systemd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"

	"github.com/PeterSR/systemd-whilst/presets"
	"github.com/PeterSR/systemd-whilst/systemd/action"
	"github.com/PeterSR/systemd-whilst/systemd/bus"
	"github.com/PeterSR/systemd-whilst/systemd/output"
	"github.com/PeterSR/systemd-whilst/systemd/source"
	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/plugin"
)

// Registry returns the config registry with every fact kind, action kind and
// preset whilst provides.
func Registry() *config.Registry {
	reg := config.NewRegistry()
	source.Register(reg)
	action.Register(reg)
	reg.Presets = presets.Files
	reg.Messages = config.Messages{
		ActionExample: `{ notify = "..." }`,
		TempRules:     "`whilst ensure`",
	}
	return reg
}

// Outputs returns the outputs, applied through hub.
func Outputs(hub *bus.Hub) []plugin.Output {
	return []plugin.Output{
		output.NewInhibit(output.LogindLocker{Hub: hub}),
		output.NewUnits(output.Manager{Hub: hub, Bus: bus.Session}, output.Manager{Hub: hub, Bus: bus.System}),
	}
}

// OfflineOutputs are output instances used only to parse and validate rule
// values, never to apply them.
func OfflineOutputs() []plugin.Output {
	return []plugin.Output{output.NewInhibit(nil), output.NewUnits(nil, nil)}
}

// NewSource returns the engine's source factory for facts read through hub.
func NewSource(hub *bus.Hub) func(*config.Fact) (plugin.Source, error) {
	return func(f *config.Fact) (plugin.Source, error) { return source.New(f, hub) }
}

// NewRunner starts the action runner; it stops when ctx is cancelled.
func NewRunner(ctx context.Context, hub *bus.Hub) plugin.Runner {
	return action.NewBusRunner(ctx, hub)
}

// Lifetimes ties temporary rules to clients on the session bus: a holder is
// a client's unique bus name, and it is gone when that name has no owner.
type Lifetimes struct{ Hub *bus.Hub }

func (l Lifetimes) Watch(holder string, gone func()) (func(), error) {
	return l.Hub.WatchOwner(bus.Session, holder, func(newOwner string) {
		if newOwner == "" {
			gone()
		}
	})
}

func (l Lifetimes) Alive(holder string) bool { return l.Hub.NameHasOwner(bus.Session, holder) }

// ConfigPath returns the config path whilst uses when none is given.
func ConfigPath() string {
	if p := os.Getenv("WHILST_CONFIG"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(dir) {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "whilst", "config.toml")
}

// StatePath is where state is kept when nothing else is configured.
func StatePath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(dir) {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "whilst", "state.json")
}

// BootID returns the kernel's id for the current boot.
func BootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(b))
}
