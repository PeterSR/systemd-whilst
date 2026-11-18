package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/PeterSR/systemd-whilst/systemd/bus"
	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/plugin"
)

// Systemd is the slice of the systemd manager API the units output needs. It
// is an interface so tests can fake systemd.
type Systemd interface {
	ActiveState(unit string) (string, error)
	Start(unit string) error
	Stop(unit string) error
}

// Manager talks to a systemd manager over D-Bus: the per-user one on the
// session bus, or the system one (PID 1) on the system bus. On the system
// bus, polkit decides whether this user may start and stop a unit.
type Manager struct {
	Hub *bus.Hub
	Bus string // bus.Session or bus.System
}

func (s Manager) conn() (*dbus.Conn, error) { return s.Hub.Conn(s.Bus) }

const systemdDest = "org.freedesktop.systemd1"
const systemdPath = "/org/freedesktop/systemd1"

func (s Manager) ActiveState(unit string) (string, error) {
	c, err := s.conn()
	if err != nil {
		return "", err
	}
	var p dbus.ObjectPath
	if err := bus.Call(c, systemdDest, systemdPath, "org.freedesktop.systemd1.Manager.LoadUnit", unit).Store(&p); err != nil {
		return "", err
	}
	v, err := bus.GetProperty(c, systemdDest, string(p), "org.freedesktop.systemd1.Unit", "ActiveState")
	if err != nil {
		return "", err
	}
	st, _ := v.Value().(string)
	return st, nil
}

func (s Manager) Start(unit string) error {
	c, err := s.conn()
	if err != nil {
		return err
	}
	return bus.Call(c, systemdDest, systemdPath, "org.freedesktop.systemd1.Manager.StartUnit", unit, "replace").Err
}

func (s Manager) Stop(unit string) error {
	c, err := s.conn()
	if err != nil {
		return err
	}
	return bus.Call(c, systemdDest, systemdPath, "org.freedesktop.systemd1.Manager.StopUnit", unit, "replace").Err
}

// Units keeps units running while rules want them: user units on the
// session bus, and system units on the system bus where polkit allows it.
//
// When a unit becomes wanted it is started, unless it is already running, in
// which case whilst does not own it and will not stop it later. When it is no
// longer wanted, it is stopped only if whilst started it. A unit that stops
// while still wanted (a crash, or someone stopping it by hand) is left alone:
// restarting crashed services is systemd's job (Restart=), and fighting a
// manual stop would be rude. It is started again once no rule wants it and
// then one does again. The same holds on both buses, and a unit name on one
// bus is a different unit from the same name on the other.
type Units struct {
	user, system Systemd

	// claimed maps units currently wanted and acted on to whether whilst
	// started them (and so may stop them). Keys are unit keys (see
	// unitKey), as are the keys of wanted and errs.
	claimed map[string]bool
	wanted  map[string][]string
	errs    map[string]string
}

// NewUnits returns the units output, managing user units through user and
// system units through system.
func NewUnits(user, system Systemd) *Units {
	return &Units{user: user, system: system, claimed: map[string]bool{}, errs: map[string]string{}}
}

func (*Units) Name() string { return "units" }

// systemKeyPrefix marks a system unit's key. A unit key is what the units
// output uses for a unit in its claims, in memory and in its saved state: a
// user unit is keyed by its plain name, as whilst has always saved it, and a
// system unit by "system/" followed by its name. A unit name cannot contain
// "/" (bus.ValidUnitName rejects it, as systemd does), so no user unit key,
// including every one in state saved before system units existed, can be
// read as a system unit's key. Status shows a system unit as
// "system:NAME" instead (see displayKey).
const systemKeyPrefix = "system/"

// unitKey returns the key of the unit name on the bus busName.
func unitKey(busName, name string) string {
	if busName == bus.System {
		return systemKeyPrefix + name
	}
	return name
}

// splitKey returns the bus and the unit name of a unit key.
func splitKey(key string) (busName, name string) {
	if n, ok := strings.CutPrefix(key, systemKeyPrefix); ok {
		return bus.System, n
	}
	return bus.Session, key
}

// displayKey is how status and errors name the unit of a key: a user unit
// by its name, as always, and a system unit as "system:NAME".
func displayKey(key string) string {
	if b, n := splitKey(key); b == bus.System {
		return "system:" + n
	}
	return key
}

// Parse accepts a unit name, a table { name, bus }, or a list of either. A
// plain name is a user unit; bus is "user" (or "session") or "system", as for
// the unit fact kind. The claim is the list of unit keys.
func (*Units) Parse(raw any) (any, error) {
	var elems []any
	switch x := raw.(type) {
	case []any:
		elems = x
	case map[string]any:
		elems = []any{x}
	default:
		// A name or a list of names, with the errors whilst has always
		// given for anything else.
		list, err := config.StringList(raw)
		if err != nil {
			return nil, fmt.Errorf("units: %w", err)
		}
		for _, s := range list {
			elems = append(elems, s)
		}
	}
	if len(elems) == 0 {
		return nil, fmt.Errorf("units: list at least one unit")
	}
	for _, e := range elems {
		switch e.(type) {
		case string, map[string]any:
		default:
			// The message whilst has always given; tables are allowed too.
			return nil, fmt.Errorf("units: expected a list of strings")
		}
	}
	keys := make([]string, 0, len(elems))
	for _, e := range elems {
		var name, busName string
		switch x := e.(type) {
		case string:
			name = x
		case map[string]any:
			if err := config.Fields(x, map[string]*string{"name": &name, "bus": &busName}, "name"); err != nil {
				return nil, fmt.Errorf("units: %w", err)
			}
		}
		if err := bus.ValidUnitName(name); err != nil {
			return nil, fmt.Errorf("units: %w", err)
		}
		b, err := bus.Normalize(busName, bus.Session)
		if err != nil {
			return nil, fmt.Errorf("units: %w", err)
		}
		keys = append(keys, unitKey(b, name))
	}
	return keys, nil
}

// manager returns the systemd manager for a unit key, and the unit name.
func (u *Units) manager(key string) (Systemd, string, error) {
	b, name := splitKey(key)
	sd := u.user
	if b == bus.System {
		sd = u.system
	}
	if sd == nil {
		return nil, name, fmt.Errorf("no systemd manager on the %s bus", b)
	}
	return sd, name, nil
}

// explain turns a refusal from the system bus into a message that says what
// to do about it. Other errors are returned as they are.
func explain(key string, err error) error {
	if b, _ := splitKey(key); b != bus.System {
		return err
	}
	name := ""
	var pe *dbus.Error
	var ve dbus.Error
	switch {
	case errors.As(err, &pe):
		name = pe.Name
	case errors.As(err, &ve):
		name = ve.Name
	}
	switch name {
	case "org.freedesktop.DBus.Error.AccessDenied", "org.freedesktop.DBus.Error.InteractiveAuthorizationRequired":
		return fmt.Errorf("not allowed: needs a polkit rule for this unit (%w)", err)
	}
	return err
}

func (u *Units) Reconcile(wants []plugin.Want) error {
	want := union(wants)
	u.wanted = want
	var errs []error
	fail := func(key, verb string, err error) {
		err = explain(key, err)
		u.errs[key] = err.Error()
		if verb != "" {
			verb += " "
		}
		errs = append(errs, fmt.Errorf("%s%s: %w", verb, displayKey(key), err))
	}
	for _, key := range sortedKeys(want) {
		if _, ok := u.claimed[key]; ok {
			continue
		}
		sd, name, err := u.manager(key)
		if err != nil {
			fail(key, "", err)
			continue
		}
		state, err := sd.ActiveState(name)
		if err != nil {
			fail(key, "", err)
			continue
		}
		if state == "active" || state == "activating" || state == "reloading" || state == "refreshing" {
			u.claimed[key] = false
			delete(u.errs, key)
			continue
		}
		if err := sd.Start(name); err != nil {
			fail(key, "start", err)
			continue
		}
		u.claimed[key] = true
		delete(u.errs, key)
	}
	for _, key := range sortedClaimed(u.claimed) {
		if _, ok := want[key]; ok {
			continue
		}
		if u.claimed[key] {
			sd, name, err := u.manager(key)
			if err == nil {
				err = sd.Stop(name)
			}
			if err != nil {
				fail(key, "stop", err)
				continue
			}
		}
		delete(u.claimed, key)
		delete(u.errs, key)
	}
	for key := range u.errs {
		if _, w := want[key]; !w {
			if _, c := u.claimed[key]; !c {
				delete(u.errs, key)
			}
		}
	}
	return errors.Join(errs...)
}

func (u *Units) Status() []plugin.Item {
	keys := map[string]bool{}
	for k := range u.claimed {
		keys[k] = true
	}
	for k := range u.wanted {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	var items []plugin.Item
	for _, n := range names {
		owned, held := u.claimed[n]
		it := plugin.Item{Key: displayKey(n), Held: held, WantedBy: u.wanted[n], Err: u.errs[n]}
		switch {
		case held && owned:
			it.Note = "started by whilst"
		case held:
			it.Note = "was already running; whilst will not stop it"
		}
		items = append(items, it)
	}
	return items
}

func (u *Units) Save() json.RawMessage {
	if len(u.claimed) == 0 {
		return nil
	}
	b, _ := json.Marshal(u.claimed)
	return b
}

func (u *Units) Load(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	m := map[string]bool{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	u.claimed = m
	return nil
}

// Units keep running across a restart of whilst; ownership is persisted so
// the next run can stop them when they are no longer wanted.
func (*Units) Close() {}

func sortedClaimed(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
