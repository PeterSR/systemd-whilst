package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/PeterSR/systemd-whilst/internal/bus"
	"github.com/PeterSR/systemd-whilst/internal/value"
)

// Action is one entry in a rule's enter or exit list. Exactly one field is set.
type Action struct {
	Notify   *Notify
	Command  *ActionCommand
	DBusCall *DBusCall
	// Set writes state facts.
	Set map[string]any
}

// Kind names the action for logs.
func (a Action) Kind() string {
	switch {
	case a.Notify != nil:
		return "notify"
	case a.Command != nil:
		return "command"
	case a.DBusCall != nil:
		return "dbus_call"
	case a.Set != nil:
		return "set"
	}
	return "?"
}

// Notify shows a desktop notification.
type Notify struct {
	Summary string
	Body    string
	Urgency string // low, normal, critical
}

// ActionCommand runs a shell command.
type ActionCommand struct {
	Run     string
	Timeout time.Duration
}

// DBusCall calls a D-Bus method.
type DBusCall struct {
	Bus       string
	Service   string
	Path      string
	Interface string
	Method    string
	Signature string
	Args      []any
}

func parseActions(v any) ([]Action, error) {
	var list []map[string]any
	switch x := v.(type) {
	case []map[string]any:
		list = x
	case map[string]any:
		list = []map[string]any{x}
	case []any:
		for _, e := range x {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("each action must be a table, like { notify = \"...\" }")
			}
			list = append(list, m)
		}
	default:
		return nil, fmt.Errorf("must be a list of action tables")
	}
	out := make([]Action, 0, len(list))
	for i, m := range list {
		a, err := parseAction(m)
		if err != nil {
			return nil, fmt.Errorf("action %d: %w", i+1, err)
		}
		out = append(out, a)
	}
	return out, nil
}

func parseAction(m map[string]any) (Action, error) {
	var a Action
	var kinds []string
	for _, k := range []string{"notify", "command", "dbus_call", "set"} {
		if _, ok := m[k]; ok {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) != 1 {
		return a, fmt.Errorf("needs exactly one of notify, command, dbus_call or set")
	}
	kind := kinds[0]
	for k := range m {
		if k != kind && !(kind == "command" && k == "timeout") {
			return a, fmt.Errorf("unknown key %q in a %s action", k, kind)
		}
	}
	switch kind {
	case "notify":
		n := &Notify{Urgency: "normal"}
		switch x := m[kind].(type) {
		case string:
			n.Summary = x
		case map[string]any:
			if err := fields(x, map[string]*string{"summary": &n.Summary, "body": &n.Body, "urgency": &n.Urgency}, "summary"); err != nil {
				return a, fmt.Errorf("notify: %w", err)
			}
		default:
			return a, fmt.Errorf("notify must be a string or a table with summary, body and urgency")
		}
		if n.Summary == "" {
			return a, fmt.Errorf("notify needs a summary")
		}
		if n.Urgency != "low" && n.Urgency != "normal" && n.Urgency != "critical" {
			return a, fmt.Errorf("notify urgency must be low, normal or critical")
		}
		a.Notify = n
	case "command":
		run, ok := m[kind].(string)
		if !ok || strings.TrimSpace(run) == "" {
			return a, fmt.Errorf("command must be a non-empty string")
		}
		c := &ActionCommand{Run: run, Timeout: time.Minute}
		if v, ok := m["timeout"]; ok {
			d, err := durationValue(v)
			if err != nil || d <= 0 {
				return a, fmt.Errorf("timeout must be a positive duration")
			}
			c.Timeout = d
		}
		a.Command = c
	case "dbus_call":
		t, ok := m[kind].(map[string]any)
		if !ok {
			return a, fmt.Errorf("dbus_call must be a table")
		}
		d := &DBusCall{}
		var args any
		if v, ok := t["args"]; ok {
			args = v
			delete(t, "args")
		}
		if err := fields(t, map[string]*string{"bus": &d.Bus, "service": &d.Service, "path": &d.Path, "interface": &d.Interface, "method": &d.Method, "signature": &d.Signature}, "service", "path", "method"); err != nil {
			return a, fmt.Errorf("dbus_call: %w", err)
		}
		if d.Interface == "" {
			dot := strings.LastIndexByte(d.Method, '.')
			if dot <= 0 {
				return a, fmt.Errorf("dbus_call: give interface, or a method with its interface like org.freedesktop.login1.Manager.Suspend")
			}
			d.Interface, d.Method = d.Method[:dot], d.Method[dot+1:]
		}
		var err error
		if d.Bus, err = bus.Normalize(d.Bus, bus.System); err != nil {
			return a, fmt.Errorf("dbus_call: %w", err)
		}
		for _, chk := range []error{bus.ValidBusName(d.Service), bus.ValidPath(d.Path), bus.ValidInterface(d.Interface), bus.ValidMember(d.Method)} {
			if chk != nil {
				return a, fmt.Errorf("dbus_call: %w", chk)
			}
		}
		if args != nil {
			list, ok := args.([]any)
			if !ok {
				return a, fmt.Errorf("dbus_call: args must be a list")
			}
			for _, e := range list {
				d.Args = append(d.Args, value.Normalize(e))
			}
		}
		if _, err := ConvertArgs(d.Signature, d.Args); err != nil {
			return a, fmt.Errorf("dbus_call: %w", err)
		}
		a.DBusCall = d
	case "set":
		t, ok := m[kind].(map[string]any)
		if !ok || len(t) == 0 {
			return a, fmt.Errorf("set must be a table of fact = value")
		}
		a.Set = map[string]any{}
		for k, v := range t {
			a.Set[k] = value.Normalize(v)
		}
	}
	return a, nil
}
