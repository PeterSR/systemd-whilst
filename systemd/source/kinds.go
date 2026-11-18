package source

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/PeterSR/systemd-whilst/systemd/bus"
	"github.com/PeterSR/whilst/config"
)

// Fact source kinds this package provides.
const (
	SrcDBus       = "dbus"
	SrcDBusSignal = "dbus_signal"
	SrcUnit       = "unit"
	SrcCommand    = "command"
	SrcFile       = "file"
)

// Spec is the parsed source of a dbus, dbus_signal, unit, command or file
// fact. Exactly one of the source fields is set, matching Kind.
type Spec struct {
	Kind string

	// Re-read triggers, for sources that can be re-read.
	OnDBus        []bus.Match // system bus
	OnDBusSession []bus.Match
	Every         time.Duration

	DBus       *DBusProp
	DBusSignal *DBusSignal
	Unit       *Unit
	Command    *Command
	File       string
}

// DBusProp reads a D-Bus property.
type DBusProp struct {
	Bus, Service, Path, Interface, Property string
}

// DBusSignal takes its value from a signal.
type DBusSignal struct {
	Bus   string
	Match bus.Match
	// Arg selects one argument of the signal body; -1 means the whole body
	// (or its only element when it has exactly one).
	Arg int
}

// Unit reads a systemd unit's ActiveState.
type Unit struct {
	Bus  string
	Name string
}

// Command runs a shell command.
type Command struct {
	Run     string
	Output  string // "stdout", "status" or "json"
	Timeout time.Duration
}

// SourceKey keeps the layout whilst has always stored in its state file: the
// kind, then one slot each for dbus, dbus_signal, unit, command, file, expr
// and state. Triggers and a command's timeout do not change the value, so
// they are left out.
func (s *Spec) SourceKey() string {
	var cmd *Command
	if s.Command != nil {
		c := *s.Command
		c.Timeout = 0
		cmd = &c
	}
	b, _ := json.Marshal([]any{s.Kind, s.DBus, s.DBusSignal, s.Unit, cmd, s.File, "", nil})
	return string(b)
}

// triggerKeys are the generic re-read triggers of sources that can be re-read.
var triggerKeys = []string{"on_dbus", "on_dbus_session", "every"}

// Register adds the fact kinds of this package to reg, in the order whilst
// lists them.
func Register(reg *config.Registry) {
	reg.AddFactKind(config.FactKind{Name: SrcDBus, Keys: triggerKeys, Rereadable: true, Parse: parseDBus})
	reg.AddFactKind(config.FactKind{Name: SrcDBusSignal, Parse: parseDBusSignal})
	reg.AddFactKind(config.FactKind{Name: SrcUnit, Keys: triggerKeys, Rereadable: true, Parse: parseUnit})
	reg.AddFactKind(config.FactKind{Name: SrcCommand, Keys: append([]string{"output", "timeout"}, triggerKeys...), Rereadable: true, Parse: parseCommand})
	reg.AddFactKind(config.FactKind{Name: SrcFile, Keys: triggerKeys, Rereadable: true, Parse: parseFile})
}

// parseTriggers reads on_dbus, on_dbus_session and every.
func parseTriggers(kind string, t map[string]any) (*Spec, error) {
	s := &Spec{Kind: kind}
	var err error
	if v, ok := t["every"]; ok {
		if s.Every, err = config.Duration(v); err != nil {
			return nil, fmt.Errorf("every: %w", err)
		}
		if s.Every < time.Second {
			return nil, fmt.Errorf("every must be at least 1s")
		}
	}
	for key, dst := range map[string]*[]bus.Match{"on_dbus": &s.OnDBus, "on_dbus_session": &s.OnDBusSession} {
		v, ok := t[key]
		if !ok {
			continue
		}
		rules, err := config.StringList(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		for _, r := range rules {
			m, err := bus.ParseMatch(r)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			*dst = append(*dst, m)
		}
	}
	return s, nil
}

func parseDBus(src any, t map[string]any) (config.FactSpec, error) {
	s, err := parseTriggers(SrcDBus, t)
	if err != nil {
		return nil, err
	}
	tbl, ok := src.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("dbus must be a table with service, path, interface and property")
	}
	p := &DBusProp{}
	if err := config.Fields(tbl, map[string]*string{"bus": &p.Bus, "service": &p.Service, "path": &p.Path, "interface": &p.Interface, "property": &p.Property}, "service", "path", "interface", "property"); err != nil {
		return nil, fmt.Errorf("dbus: %w", err)
	}
	if p.Bus, err = bus.Normalize(p.Bus, bus.System); err != nil {
		return nil, fmt.Errorf("dbus: %w", err)
	}
	for _, chk := range []error{bus.ValidBusName(p.Service), bus.ValidPath(p.Path), bus.ValidInterface(p.Interface), bus.ValidMember(p.Property)} {
		if chk != nil {
			return nil, fmt.Errorf("dbus: %w", chk)
		}
	}
	s.DBus = p
	return s, nil
}

func parseDBusSignal(src any, _ map[string]any) (config.FactSpec, error) {
	tbl, ok := src.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("dbus_signal must be a table")
	}
	s := &DBusSignal{Arg: -1}
	var rule string
	var arg any
	if a, ok := tbl["arg"]; ok {
		arg = a
		delete(tbl, "arg")
	}
	m := bus.Match{Type: "signal"}
	if err := config.Fields(tbl, map[string]*string{"bus": &s.Bus, "match": &rule, "sender": &m.Sender, "path": &m.Path, "interface": &m.Interface, "member": &m.Member}); err != nil {
		return nil, fmt.Errorf("dbus_signal: %w", err)
	}
	var err error
	if rule != "" {
		if m.Sender != "" || m.Path != "" || m.Interface != "" || m.Member != "" {
			return nil, fmt.Errorf("dbus_signal: give either match or sender/path/interface/member, not both")
		}
		if m, err = bus.ParseMatch(rule); err != nil {
			return nil, fmt.Errorf("dbus_signal: %w", err)
		}
	}
	if m.Member == "" {
		return nil, fmt.Errorf("dbus_signal: member is required")
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("dbus_signal: %w", err)
	}
	s.Match = m
	if arg != nil {
		i, ok := arg.(int64)
		if !ok || i < 0 || i > 63 {
			return nil, fmt.Errorf("dbus_signal: arg must be a non-negative integer")
		}
		s.Arg = int(i)
	}
	if s.Bus, err = bus.Normalize(s.Bus, bus.System); err != nil {
		return nil, fmt.Errorf("dbus_signal: %w", err)
	}
	return &Spec{Kind: SrcDBusSignal, DBusSignal: s}, nil
}

func parseUnit(src any, t map[string]any) (config.FactSpec, error) {
	s, err := parseTriggers(SrcUnit, t)
	if err != nil {
		return nil, err
	}
	u := &Unit{}
	switch x := src.(type) {
	case string:
		u.Name = x
	case map[string]any:
		if err := config.Fields(x, map[string]*string{"name": &u.Name, "bus": &u.Bus}, "name"); err != nil {
			return nil, fmt.Errorf("unit: %w", err)
		}
	default:
		return nil, fmt.Errorf("unit must be a unit name or a table with name and bus")
	}
	if err := bus.ValidUnitName(u.Name); err != nil {
		return nil, fmt.Errorf("unit: %w", err)
	}
	if u.Bus, err = bus.Normalize(u.Bus, bus.Session); err != nil {
		return nil, fmt.Errorf("unit: %w", err)
	}
	s.Unit = u
	return s, nil
}

func parseCommand(src any, t map[string]any) (config.FactSpec, error) {
	s, err := parseTriggers(SrcCommand, t)
	if err != nil {
		return nil, err
	}
	run, ok := src.(string)
	if !ok || strings.TrimSpace(run) == "" {
		return nil, fmt.Errorf("command must be a non-empty string")
	}
	c := &Command{Run: run, Output: "stdout", Timeout: 10 * time.Second}
	if v, ok := t["output"]; ok {
		o, _ := v.(string)
		if o != "stdout" && o != "status" && o != "json" {
			return nil, fmt.Errorf("output must be stdout, status or json")
		}
		c.Output = o
	}
	if v, ok := t["timeout"]; ok {
		if c.Timeout, err = config.Duration(v); err != nil || c.Timeout <= 0 {
			return nil, fmt.Errorf("timeout must be a positive duration")
		}
	}
	s.Command = c
	return s, nil
}

func parseFile(src any, t map[string]any) (config.FactSpec, error) {
	s, err := parseTriggers(SrcFile, t)
	if err != nil {
		return nil, err
	}
	p, ok := src.(string)
	if !ok || p == "" {
		return nil, fmt.Errorf("file must be a path")
	}
	s.File = config.ExpandHome(p)
	if s.Every == 0 {
		s.Every = 5 * time.Second
	}
	return s, nil
}
