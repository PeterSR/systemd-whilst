package output

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/PeterSR/whilst/plugin"
)

func TestUnitsParse(t *testing.T) {
	u := NewUnits(nil, nil)
	for _, c := range []struct {
		raw  any
		want []string
	}{
		{"a.service", []string{"a.service"}},
		{[]any{"a.service", "b.timer"}, []string{"a.service", "b.timer"}},
		{[]string{"a.service"}, []string{"a.service"}},
		{map[string]any{"name": "a.service", "bus": "system"}, []string{"system/a.service"}},
		{[]any{map[string]any{"name": "a.service"}}, []string{"a.service"}},
		{[]any{map[string]any{"name": "a.service", "bus": "user"}}, []string{"a.service"}},
		{[]any{map[string]any{"name": "a.service", "bus": "session"}}, []string{"a.service"}},
		{[]any{"a.service", map[string]any{"name": "a.service", "bus": "system"}}, []string{"a.service", "system/a.service"}},
	} {
		got, err := u.Parse(c.raw)
		if err != nil {
			t.Errorf("%v: %v", c.raw, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: got %v, want %v", c.raw, got, c.want)
		}
	}
	for _, c := range []struct {
		raw  any
		want string
	}{
		// The messages whilst has always given.
		{int64(1), "units: expected a string or a list of strings"},
		{[]any{"a.service", int64(1)}, "units: expected a list of strings"},
		{[]any{}, "units: list at least one unit"},
		{[]any{"foo"}, `units: "foo" needs a unit type suffix, like .service or .timer`},
		{"a/b.service", `units: "a/b.service" is not a valid unit name`},
		// Tables.
		{[]any{map[string]any{"name": "a.service", "bus": "nope"}}, `units: unknown bus "nope" (use system, session or user)`},
		{[]any{map[string]any{"name": "a.service", "unit": "x"}}, `units: unknown key "unit"`},
		{[]any{map[string]any{"bus": "system"}}, "units: name is required"},
		{[]any{map[string]any{"name": int64(1)}}, "units: name must be a string"},
		{[]any{map[string]any{"name": "foo", "bus": "system"}}, `units: "foo" needs a unit type suffix, like .service or .timer`},
	} {
		_, err := u.Parse(c.raw)
		if err == nil || err.Error() != c.want {
			t.Errorf("%v: error %v, want %q", c.raw, err, c.want)
		}
	}
}

type fakeManager struct {
	active  map[string]bool
	started []string
	stopped []string
	fail    error
}

func newFake(active ...string) *fakeManager {
	f := &fakeManager{active: map[string]bool{}}
	for _, a := range active {
		f.active[a] = true
	}
	return f
}

func (s *fakeManager) ActiveState(u string) (string, error) {
	if s.active[u] {
		return "active", nil
	}
	return "inactive", nil
}

func (s *fakeManager) Start(u string) error {
	if s.fail != nil {
		return s.fail
	}
	s.active[u] = true
	s.started = append(s.started, u)
	return nil
}

func (s *fakeManager) Stop(u string) error {
	if s.fail != nil {
		return s.fail
	}
	s.active[u] = false
	s.stopped = append(s.stopped, u)
	return nil
}

func wants(t *testing.T, u *Units, raw any) []plugin.Want {
	t.Helper()
	c, err := u.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return []plugin.Want{{Rule: "r", Claim: c}}
}

func statusLines(u *Units) string {
	var out []string
	for _, it := range u.Status() {
		s := it.Key + "|" + it.Note
		if it.Held {
			s += "|held"
		}
		if it.Err != "" {
			s += "|err " + it.Err
		}
		out = append(out, s)
	}
	return strings.Join(out, "\n")
}

// The same name on both buses is two units, each owned on its own.
func TestUnitsOwnershipPerBus(t *testing.T) {
	user, sys := newFake(), newFake("b.service")
	u := NewUnits(user, sys)
	both := []any{"a.service", "b.service", map[string]any{"name": "a.service", "bus": "system"}, map[string]any{"name": "b.service", "bus": "system"}}
	if err := u.Reconcile(wants(t, u, both)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(user.started, ",") != "a.service,b.service" || strings.Join(sys.started, ",") != "a.service" {
		t.Fatalf("started user %v, system %v", user.started, sys.started)
	}
	want := strings.Join([]string{
		"a.service|started by whilst|held",
		"b.service|started by whilst|held",
		"system:a.service|started by whilst|held",
		"system:b.service|was already running; whilst will not stop it|held",
	}, "\n")
	if got := statusLines(u); got != want {
		t.Fatalf("status:\n%s\nwant:\n%s", got, want)
	}

	// The system a.service is stopped by hand: whilst does not fight it,
	// and the user a.service is not affected.
	sys.active["a.service"] = false
	if err := u.Reconcile(wants(t, u, both)); err != nil {
		t.Fatal(err)
	}
	if len(sys.started) != 1 || len(user.started) != 2 {
		t.Fatalf("restarted: user %v, system %v", user.started, sys.started)
	}

	// Only the user units are still wanted: whilst stops the one system
	// unit it started, and leaves the one that was already running.
	if err := u.Reconcile(wants(t, u, []any{"a.service", "b.service"})); err != nil {
		t.Fatal(err)
	}
	if len(user.stopped) != 0 || strings.Join(sys.stopped, ",") != "a.service" || !sys.active["b.service"] {
		t.Fatalf("stopped user %v, system %v", user.stopped, sys.stopped)
	}
	if err := u.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(user.stopped, ",") != "a.service,b.service" || len(sys.stopped) != 1 {
		t.Fatalf("stopped user %v, system %v", user.stopped, sys.stopped)
	}
	if got := statusLines(u); got != "" {
		t.Fatalf("status after release:\n%s", got)
	}
}

func TestUnitsSystemDenied(t *testing.T) {
	for _, name := range []string{"org.freedesktop.DBus.Error.AccessDenied", "org.freedesktop.DBus.Error.InteractiveAuthorizationRequired"} {
		for _, e := range []error{dbus.Error{Name: name, Body: []any{"denied"}}, dbus.NewError(name, []any{"denied"})} {
			user, sys := newFake(), newFake()
			sys.fail = e
			u := NewUnits(user, sys)
			w := wants(t, u, []any{"a.service", map[string]any{"name": "a.service", "bus": "system"}})
			err := u.Reconcile(w)
			if err == nil || err.Error() != "start system:a.service: not allowed: needs a polkit rule for this unit (denied)" {
				t.Fatalf("%s: error %v", name, err)
			}
			want := "a.service|started by whilst|held\nsystem:a.service||err not allowed: needs a polkit rule for this unit (denied)"
			if got := statusLines(u); got != want {
				t.Fatalf("%s: status:\n%s", name, got)
			}
			// Retried on the next reconcile; once allowed, it is started.
			sys.fail = nil
			if err := u.Reconcile(w); err != nil {
				t.Fatal(err)
			}
			if strings.Join(sys.started, ",") != "a.service" || strings.Contains(statusLines(u), "err") {
				t.Fatalf("after retry: started %v, status\n%s", sys.started, statusLines(u))
			}
			// A denied stop keeps the unit owned, to try again.
			sys.fail = e
			if err := u.Reconcile(nil); err == nil || !strings.Contains(err.Error(), "stop system:a.service: not allowed") {
				t.Fatalf("stop error %v", err)
			}
			sys.fail = nil
			if err := u.Reconcile(nil); err != nil || strings.Join(sys.stopped, ",") != "a.service" {
				t.Fatalf("stop retry: %v, stopped %v", err, sys.stopped)
			}
		}
	}

	// Other errors, and errors from the user bus, are shown as they are.
	user, sys := newFake(), newFake()
	user.fail = dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied", Body: []any{"denied"}}
	sys.fail = errors.New("boom")
	u := NewUnits(user, sys)
	err := u.Reconcile(wants(t, u, []any{"a.service", map[string]any{"name": "a.service", "bus": "system"}}))
	if err == nil || err.Error() != "start a.service: denied\nstart system:a.service: boom" {
		t.Fatalf("error %v", err)
	}
}

func TestUnitsSaveLoad(t *testing.T) {
	// State saved before system units existed: user units by name.
	old := `{"a.service":true,"b.service":false}`
	user, sys := newFake("a.service", "b.service"), newFake()
	u := NewUnits(user, sys)
	if err := u.Load([]byte(old)); err != nil {
		t.Fatal(err)
	}
	if got := string(u.Save()); got != old {
		t.Fatalf("saved %s, want the same bytes as loaded", got)
	}
	if err := u.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(user.stopped, ",") != "a.service" || len(sys.stopped) != 0 {
		t.Fatalf("stopped user %v, system %v", user.stopped, sys.stopped)
	}

	// Both buses round trip, and the same name on each stays apart.
	user, sys = newFake(), newFake("c.service")
	u = NewUnits(user, sys)
	w := wants(t, u, []any{"a.service", map[string]any{"name": "a.service", "bus": "system"}, map[string]any{"name": "c.service", "bus": "system"}})
	if err := u.Reconcile(w); err != nil {
		t.Fatal(err)
	}
	saved := u.Save()
	if string(saved) != `{"a.service":true,"system/a.service":true,"system/c.service":false}` {
		t.Fatalf("saved %s", saved)
	}
	u2 := NewUnits(user, sys)
	if err := u2.Load(saved); err != nil {
		t.Fatal(err)
	}
	if err := u2.Reconcile(wants(t, u2, "a.service")); err != nil {
		t.Fatal(err)
	}
	if len(user.stopped) != 0 || strings.Join(sys.stopped, ",") != "a.service" || !sys.active["c.service"] {
		t.Fatalf("stopped user %v, system %v", user.stopped, sys.stopped)
	}
}
