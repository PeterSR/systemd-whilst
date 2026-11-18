package action

import (
	"fmt"
	"strings"
	"time"

	"github.com/PeterSR/systemd-whilst/systemd/bus"
	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/value"
)

// Notify shows a desktop notification.
type Notify struct {
	Summary string
	Body    string
	Urgency string // low, normal, critical
}

// Command runs a shell command.
type Command struct {
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

// Register adds the action kinds of this package to reg. The hash names and
// the order keep the names whilst has always given unnamed rules.
func Register(reg *config.Registry) {
	reg.AddActionKind(config.ActionKind{Name: "notify", LegacyHashSlot: "Notify", Parse: parseNotify})
	reg.AddActionKind(config.ActionKind{Name: "command", LegacyHashSlot: "Command", Keys: []string{"timeout"}, Parse: parseCommand})
	reg.AddActionKind(config.ActionKind{Name: "dbus_call", LegacyHashSlot: "DBusCall", Parse: parseDBusCall})
}

func parseNotify(v any, _ map[string]any) (any, error) {
	n := &Notify{Urgency: "normal"}
	switch x := v.(type) {
	case string:
		n.Summary = x
	case map[string]any:
		if err := config.Fields(x, map[string]*string{"summary": &n.Summary, "body": &n.Body, "urgency": &n.Urgency}, "summary"); err != nil {
			return nil, fmt.Errorf("notify: %w", err)
		}
	default:
		return nil, fmt.Errorf("notify must be a string or a table with summary, body and urgency")
	}
	if n.Summary == "" {
		return nil, fmt.Errorf("notify needs a summary")
	}
	if n.Urgency != "low" && n.Urgency != "normal" && n.Urgency != "critical" {
		return nil, fmt.Errorf("notify urgency must be low, normal or critical")
	}
	return n, nil
}

func parseCommand(v any, m map[string]any) (any, error) {
	run, ok := v.(string)
	if !ok || strings.TrimSpace(run) == "" {
		return nil, fmt.Errorf("command must be a non-empty string")
	}
	c := &Command{Run: run, Timeout: time.Minute}
	if v, ok := m["timeout"]; ok {
		d, err := config.Duration(v)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("timeout must be a positive duration")
		}
		c.Timeout = d
	}
	return c, nil
}

func parseDBusCall(v any, _ map[string]any) (any, error) {
	t, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("dbus_call must be a table")
	}
	d := &DBusCall{}
	var args any
	if v, ok := t["args"]; ok {
		args = v
		delete(t, "args")
	}
	if err := config.Fields(t, map[string]*string{"bus": &d.Bus, "service": &d.Service, "path": &d.Path, "interface": &d.Interface, "method": &d.Method, "signature": &d.Signature}, "service", "path", "method"); err != nil {
		return nil, fmt.Errorf("dbus_call: %w", err)
	}
	if d.Interface == "" {
		dot := strings.LastIndexByte(d.Method, '.')
		if dot <= 0 {
			return nil, fmt.Errorf("dbus_call: give interface, or a method with its interface like org.freedesktop.login1.Manager.Suspend")
		}
		d.Interface, d.Method = d.Method[:dot], d.Method[dot+1:]
	}
	var err error
	if d.Bus, err = bus.Normalize(d.Bus, bus.System); err != nil {
		return nil, fmt.Errorf("dbus_call: %w", err)
	}
	for _, chk := range []error{bus.ValidBusName(d.Service), bus.ValidPath(d.Path), bus.ValidInterface(d.Interface), bus.ValidMember(d.Method)} {
		if chk != nil {
			return nil, fmt.Errorf("dbus_call: %w", chk)
		}
	}
	if args != nil {
		list, ok := args.([]any)
		if !ok {
			return nil, fmt.Errorf("dbus_call: args must be a list")
		}
		for _, e := range list {
			d.Args = append(d.Args, value.Normalize(e))
		}
	}
	if _, err := ConvertArgs(d.Signature, d.Args); err != nil {
		return nil, fmt.Errorf("dbus_call: %w", err)
	}
	return d, nil
}
