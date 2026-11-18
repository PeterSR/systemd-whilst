package output

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/PeterSR/systemd-whilst/internal/bus"
	"github.com/PeterSR/systemd-whilst/internal/config"
)

// InhibitWhats are the lock types logind accepts.
var InhibitWhats = []string{
	"shutdown", "sleep", "idle",
	"handle-power-key", "handle-suspend-key", "handle-hibernate-key",
	"handle-lid-switch", "handle-reboot-key",
}

// Locker takes a logind inhibitor lock. It is an interface so tests can fake
// logind.
type Locker interface {
	Inhibit(what, who, why, mode string) (*os.File, error)
}

// LogindLocker takes locks from logind over the system bus.
type LogindLocker struct{ Hub *bus.Hub }

// Inhibit calls org.freedesktop.login1.Manager.Inhibit and returns the lock's
// file descriptor. The lock is held until the file is closed.
func (l LogindLocker) Inhibit(what, who, why, mode string) (*os.File, error) {
	c, err := l.Hub.Conn(bus.System)
	if err != nil {
		return nil, err
	}
	var fd dbus.UnixFD
	err = bus.Call(c, "org.freedesktop.login1", "/org/freedesktop/login1", "org.freedesktop.login1.Manager.Inhibit", what, who, why, mode).Store(&fd)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "inhibitor:"+what), nil
}

// Inhibit holds one blocking logind lock covering the union of every matching
// rule's `inhibit` list. When the union changes, the new lock is taken before
// the old one is closed, so there is never a gap.
type Inhibit struct {
	locker Locker

	file   *os.File
	whats  []string
	why    string
	wanted map[string][]string
	err    error
}

// NewInhibit returns the inhibit output.
func NewInhibit(l Locker) *Inhibit { return &Inhibit{locker: l} }

func (*Inhibit) Name() string { return "inhibit" }

func (*Inhibit) Parse(raw any) (any, error) {
	list, err := config.StringList(raw)
	if err != nil {
		return nil, fmt.Errorf("inhibit: %w", err)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("inhibit: list at least one lock type")
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range list {
		if !contains(InhibitWhats, w) {
			return nil, fmt.Errorf("inhibit: unknown lock type %q (one of %s)", w, strings.Join(InhibitWhats, ", "))
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out, nil
}

func (i *Inhibit) Reconcile(wants []Want) error {
	u := union(wants)
	i.wanted = u
	whats := sortedKeys(u)
	if len(whats) == 0 {
		i.release()
		i.err = nil
		return nil
	}
	var rules []string
	seen := map[string]bool{}
	for _, w := range whats {
		for _, r := range u[w] {
			if !seen[r] {
				seen[r] = true
				rules = append(rules, r)
			}
		}
	}
	why := "whilst: " + strings.Join(rules, ", ")
	// Re-take the lock when the rules behind it change too, so that
	// `systemd-inhibit --list` names the right ones.
	if i.file != nil && equalStrings(whats, i.whats) && why == i.why {
		i.err = nil
		return nil
	}
	f, err := i.locker.Inhibit(strings.Join(whats, ":"), "whilst", why, "block")
	if err != nil {
		// Keep the old lock: blocking too much beats blocking too little.
		i.err = fmt.Errorf("take %s lock: %w", strings.Join(whats, ":"), err)
		return i.err
	}
	old := i.file
	i.file, i.whats, i.why, i.err = f, whats, why, nil
	if old != nil {
		old.Close()
	}
	return nil
}

func (i *Inhibit) release() {
	if i.file != nil {
		i.file.Close()
	}
	i.file, i.whats, i.why = nil, nil, ""
}

func (i *Inhibit) Status() []Item {
	keys := map[string]bool{}
	for _, w := range i.whats {
		keys[w] = true
	}
	for w := range i.wanted {
		keys[w] = true
	}
	var items []Item
	for _, w := range InhibitWhats {
		if !keys[w] {
			continue
		}
		it := Item{Key: w, Held: i.file != nil && contains(i.whats, w), WantedBy: i.wanted[w]}
		if i.err != nil {
			it.Err = i.err.Error()
		}
		items = append(items, it)
	}
	return items
}

// The lock dies with the process, so there is nothing to persist.
func (*Inhibit) Save() json.RawMessage      { return nil }
func (*Inhibit) Load(json.RawMessage) error { return nil }

func (i *Inhibit) Close() { i.release() }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
