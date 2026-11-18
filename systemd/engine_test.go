package systemd_test

// Tests of whilst's real outputs (inhibitor locks, units) under the engine,
// with logind and systemd faked. They drive the engine through its public
// API; the engine's own behaviour is tested in the whilst module.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PeterSR/systemd-whilst/systemd"
	"github.com/PeterSR/systemd-whilst/systemd/output"
	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/engine"
	"github.com/PeterSR/whilst/plugin"
)

// fakeLocker records inhibitor locks.
type fakeLocker struct {
	mu    sync.Mutex
	taken []string
	files []*os.File
	fail  bool
}

func (l *fakeLocker) Inhibit(what, who, why, mode string) (*os.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail {
		return nil, os.ErrPermission
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	r.Close()
	l.taken = append(l.taken, what)
	l.files = append(l.files, w)
	return w, nil
}

// held returns the whats of locks whose file is still open.
func (l *fakeLocker) held() []string {
	var out []string
	for i, f := range l.files {
		if _, err := f.Stat(); err == nil {
			out = append(out, l.taken[i])
		}
	}
	return out
}

type fakeSystemd struct {
	active  map[string]bool
	started []string
	stopped []string
}

func (s *fakeSystemd) ActiveState(u string) (string, error) {
	if s.active[u] {
		return "active", nil
	}
	return "inactive", nil
}
func (s *fakeSystemd) Start(u string) error {
	s.active[u] = true
	s.started = append(s.started, u)
	return nil
}
func (s *fakeSystemd) Stop(u string) error {
	s.active[u] = false
	s.stopped = append(s.stopped, u)
	return nil
}

type fakeActions struct{ ran []string }

func (a *fakeActions) Run(rule, event string, acts []config.Action) {
	for _, act := range acts {
		a.ran = append(a.ran, rule+":"+event+":"+act.Kind())
	}
}

type harness struct {
	t       *testing.T
	e       *engine.Engine
	locker  *fakeLocker
	systemd *fakeSystemd
	actions *fakeActions
	now     time.Time
}

func newHarness(t *testing.T, src, statePath, bootID string, active ...string) *harness {
	t.Helper()
	cfg, err := config.Parse(src, systemd.Registry())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, locker: &fakeLocker{}, systemd: &fakeSystemd{active: map[string]bool{}}, actions: &fakeActions{}}
	for _, u := range active {
		h.systemd.active[u] = true
	}
	outs := []plugin.Output{output.NewInhibit(h.locker), output.NewUnits(h.systemd)}
	comp, err := engine.Compile(cfg, outs)
	if err != nil {
		t.Fatal(err)
	}
	h.e = engine.New(comp, engine.Deps{
		NewSource: func(*config.Fact) (plugin.Source, error) { return nil, nil },
		Outputs:   outs,
		Actions:   h.actions,
		StatePath: statePath,
		BootID:    bootID,
	})
	if err := h.e.Restore(); err != nil {
		t.Fatal(err)
	}
	h.now = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	return h
}

func (h *harness) eval() { h.e.Evaluate(h.now) }

func (h *harness) advance(d time.Duration) {
	h.now = h.now.Add(d)
	h.eval()
}

// set delivers a source value at the harness clock and evaluates.
func (h *harness) set(fact string, v any) {
	h.t.Helper()
	h.e.Apply(plugin.Update{Fact: fact, Value: v}, h.now)
	h.eval()
}

func (h *harness) heldLock() string {
	held := h.locker.held()
	if len(held) == 0 {
		return ""
	}
	if len(held) > 1 {
		h.t.Fatalf("more than one lock held: %v", held)
	}
	return held[0]
}

const sleepConfig = `
[fact.ac]
type = "bool"
command = "true"
[fact.ssid]
type = "string"
command = "true"
[fact.home]
expr = "ssid == 'HomeWifi'"

[[rule]]
name    = "home-on-mains"
while   = "ac && home"
inhibit = ["sleep", "handle-lid-switch"]

[[rule]]
name    = "just-unplugged"
while   = "!ac && since('ac') < duration('30m')"
inhibit = ["handle-lid-switch"]
enter   = [{ notify = "lid protected" }]

[[rule]]
name    = "mains-elsewhere"
while   = "ac"
inhibit = ["sleep"]
`

func TestNoGapWhenLocksChange(t *testing.T) {
	h := newHarness(t, sleepConfig, "", "boot-1")
	h.set("ssid", "HomeWifi")
	h.set("ac", true)
	h.advance(time.Minute)
	h.set("ac", false)
	// Two locks were taken in order; the second replaced the first.
	if got := strings.Join(h.locker.taken, " "); got != "handle-lid-switch:sleep handle-lid-switch" {
		t.Fatalf("taken %q", got)
	}
	if got := h.heldLock(); got != "handle-lid-switch" {
		t.Fatalf("held %q", got)
	}
}

func TestFailedLockKeepsOldAndRetries(t *testing.T) {
	h := newHarness(t, sleepConfig, "", "boot-1")
	h.set("ssid", "HomeWifi")
	h.set("ac", true)
	h.locker.fail = true
	h.advance(time.Minute)
	h.set("ac", false)
	if got := h.heldLock(); got != "handle-lid-switch:sleep" {
		t.Fatalf("old lock should be kept when the new one fails, got %q", got)
	}
	st := h.e.Snapshot()
	if st.NextWake.Sub(h.now) != 30*time.Second {
		t.Fatalf("expected a retry in 30s, next wake %v", st.NextWake.Sub(h.now))
	}
	failed := false
	for _, o := range st.Outputs {
		for _, it := range o.Items {
			failed = failed || (o.Output == "inhibit" && it.Err != "")
		}
	}
	if !failed {
		t.Fatalf("status shows no error: %+v", st.Outputs)
	}
	h.locker.fail = false
	h.advance(30 * time.Second)
	if got := h.heldLock(); got != "handle-lid-switch" {
		t.Fatalf("after retry: %q", got)
	}
}

func TestEnsureNowReportsProblems(t *testing.T) {
	h := newHarness(t, sleepConfig, "", "boot-1")
	h.set("ssid", "x")
	h.set("ac", false)
	h.locker.fail = true
	_, problems, _, err := h.e.EnsureNow(engine.TempSpec{Outputs: map[string]any{"inhibit": []any{"sleep"}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "sleep is not held") {
		t.Fatalf("problems %v", problems)
	}
	h.locker.fail = false
	_, problems, _, err = h.e.EnsureNow(engine.TempSpec{Name: "b", Outputs: map[string]any{"inhibit": []any{"idle"}}}, "")
	if err != nil || len(problems) != 0 || h.heldLock() != "idle:sleep" {
		t.Fatalf("problems %v err %v lock %q", problems, err, h.heldLock())
	}
}

func TestUnitsOwnership(t *testing.T) {
	src := `
[fact.home]
type = "bool"
command = "true"
[[rule]]
name  = "sync"
while = "home"
units = ["syncthing.service", "already.service"]
`
	h := newHarness(t, src, "", "boot-1", "already.service")
	h.set("home", true)
	if strings.Join(h.systemd.started, ",") != "syncthing.service" {
		t.Fatalf("started %v", h.systemd.started)
	}
	// Someone stops it by hand: whilst does not fight that.
	h.systemd.active["syncthing.service"] = false
	h.advance(time.Minute)
	if len(h.systemd.started) != 1 {
		t.Fatalf("restarted a unit stopped by hand: %v", h.systemd.started)
	}
	h.set("home", false)
	// Only the unit whilst started is stopped.
	if strings.Join(h.systemd.stopped, ",") != "syncthing.service" {
		t.Fatalf("stopped %v", h.systemd.stopped)
	}
	if !h.systemd.active["already.service"] {
		t.Fatal("stopped a unit that was running before whilst wanted it")
	}
}

func TestRebootResetsRulesAndUnitOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	src := `
[fact.home]
type = "bool"
command = "true"
[[rule]]
name  = "sync"
while = "home"
units = ["syncthing.service"]
enter = [{ notify = "hi" }]
`
	h := newHarness(t, src, path, "boot-1")
	h.set("home", true) // evaluating saves the state file
	if len(h.systemd.started) != 1 {
		t.Fatal("setup")
	}

	// Within the boot, a restart of whilst remembers that it started the
	// unit, and stops it when no rule wants it any more.
	h1 := newHarness(t, src, path, "boot-1", "syncthing.service")
	h1.set("home", false)
	if strings.Join(h1.systemd.stopped, ",") != "syncthing.service" || len(h1.actions.ran) != 0 {
		t.Fatalf("same boot: stopped %v, actions %v", h1.systemd.stopped, h1.actions.ran)
	}

	// After a reboot the unit is not running; whilst must start it again,
	// and the enter action runs again.
	h2 := newHarness(t, src, path, "boot-2")
	h2.set("home", true)
	if strings.Join(h2.systemd.started, ",") != "syncthing.service" {
		t.Fatalf("unit not started after reboot: %v", h2.systemd.started)
	}
	if len(h2.actions.ran) != 1 {
		t.Fatalf("enter should run again after a reboot: %v", h2.actions.ran)
	}

	// After a reboot where the unit is enabled and running but no rule
	// wants it, whilst must not stop it: it did not start it this boot.
	h3 := newHarness(t, src, path, "boot-3", "syncthing.service")
	h3.set("home", false)
	if len(h3.systemd.stopped) != 0 || len(h3.actions.ran) != 0 {
		t.Fatalf("stopped %v, actions %v", h3.systemd.stopped, h3.actions.ran)
	}
}

func TestStatusSnapshot(t *testing.T) {
	h := newHarness(t, sleepConfig, "", "boot-1")
	h.set("ssid", "HomeWifi")
	h.set("ac", true)
	st := h.e.Snapshot()
	var items []string
	for _, o := range st.Outputs {
		for _, it := range o.Items {
			items = append(items, o.Output+":"+it.Key+":"+strings.Join(it.WantedBy, "+"))
		}
	}
	want := "inhibit:sleep:home-on-mains+mains-elsewhere inhibit:handle-lid-switch:home-on-mains"
	if strings.Join(items, " ") != want {
		t.Fatalf("items %v", items)
	}
}

func TestCompileErrors(t *testing.T) {
	for _, src := range []string{
		"[[rule]]\nwhile = \"true\"\ninhbit = [\"sleep\"]\n", // typo in output
		"[[rule]]\nwhile = \"true\"\ninhibit = [\"slep\"]\n", // bad lock type
		"[[rule]]\nwhile = \"true\"\nunits = [\"foo\"]\n",    // unit without suffix
	} {
		cfg, err := config.Parse(src, systemd.Registry())
		if err != nil {
			t.Errorf("parse: %v", err)
			continue
		}
		if _, err := engine.Compile(cfg, systemd.OfflineOutputs()); err == nil {
			t.Errorf("expected an error for:\n%s", src)
		}
	}
}
