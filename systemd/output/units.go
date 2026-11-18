package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

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

// UserSystemd talks to the per-user systemd manager.
type UserSystemd struct{ Hub *bus.Hub }

func (s UserSystemd) conn() (*dbus.Conn, error) { return s.Hub.Conn(bus.Session) }

const systemdDest = "org.freedesktop.systemd1"
const systemdPath = "/org/freedesktop/systemd1"

func (s UserSystemd) ActiveState(unit string) (string, error) {
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

func (s UserSystemd) Start(unit string) error {
	c, err := s.conn()
	if err != nil {
		return err
	}
	return bus.Call(c, systemdDest, systemdPath, "org.freedesktop.systemd1.Manager.StartUnit", unit, "replace").Err
}

func (s UserSystemd) Stop(unit string) error {
	c, err := s.conn()
	if err != nil {
		return err
	}
	return bus.Call(c, systemdDest, systemdPath, "org.freedesktop.systemd1.Manager.StopUnit", unit, "replace").Err
}

// Units keeps user units running while rules want them.
//
// When a unit becomes wanted it is started, unless it is already running, in
// which case whilst does not own it and will not stop it later. When it is no
// longer wanted, it is stopped only if whilst started it. A unit that stops
// while still wanted (a crash, or someone stopping it by hand) is left alone:
// restarting crashed services is systemd's job (Restart=), and fighting a
// manual stop would be rude. It is started again once no rule wants it and
// then one does again.
type Units struct {
	sd Systemd

	// claimed maps units currently wanted and acted on to whether whilst
	// started them (and so may stop them).
	claimed map[string]bool
	wanted  map[string][]string
	errs    map[string]string
}

// NewUnits returns the units output.
func NewUnits(sd Systemd) *Units {
	return &Units{sd: sd, claimed: map[string]bool{}, errs: map[string]string{}}
}

func (*Units) Name() string { return "units" }

func (*Units) Parse(raw any) (any, error) {
	list, err := config.StringList(raw)
	if err != nil {
		return nil, fmt.Errorf("units: %w", err)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("units: list at least one unit")
	}
	for _, u := range list {
		if err := bus.ValidUnitName(u); err != nil {
			return nil, fmt.Errorf("units: %w", err)
		}
	}
	return list, nil
}

func (u *Units) Reconcile(wants []plugin.Want) error {
	want := union(wants)
	u.wanted = want
	var errs []error
	for _, name := range sortedKeys(want) {
		if _, ok := u.claimed[name]; ok {
			continue
		}
		state, err := u.sd.ActiveState(name)
		if err != nil {
			u.errs[name] = err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if state == "active" || state == "activating" || state == "reloading" || state == "refreshing" {
			u.claimed[name] = false
			delete(u.errs, name)
			continue
		}
		if err := u.sd.Start(name); err != nil {
			u.errs[name] = err.Error()
			errs = append(errs, fmt.Errorf("start %s: %w", name, err))
			continue
		}
		u.claimed[name] = true
		delete(u.errs, name)
	}
	for _, name := range sortedClaimed(u.claimed) {
		if _, ok := want[name]; ok {
			continue
		}
		if u.claimed[name] {
			if err := u.sd.Stop(name); err != nil {
				u.errs[name] = err.Error()
				errs = append(errs, fmt.Errorf("stop %s: %w", name, err))
				continue
			}
		}
		delete(u.claimed, name)
		delete(u.errs, name)
	}
	for name := range u.errs {
		if _, w := want[name]; !w {
			if _, c := u.claimed[name]; !c {
				delete(u.errs, name)
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
		it := plugin.Item{Key: n, Held: held, WantedBy: u.wanted[n], Err: u.errs[n]}
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
