package engine

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PeterSR/systemd-whilst/internal/config"
	"github.com/PeterSR/systemd-whilst/internal/output"
	"github.com/PeterSR/systemd-whilst/internal/source"
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
	e       *Engine
	locker  *fakeLocker
	systemd *fakeSystemd
	actions *fakeActions
	now     time.Time
}

func newHarness(t *testing.T, src string, statePath string) *harness {
	t.Helper()
	cfg, err := config.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, locker: &fakeLocker{}, systemd: &fakeSystemd{active: map[string]bool{}}, actions: &fakeActions{}}
	outs := []output.Output{output.NewInhibit(h.locker), output.NewUnits(h.systemd)}
	comp, err := Compile(cfg, outs)
	if err != nil {
		t.Fatal(err)
	}
	h.e = New(comp, Deps{
		NewSource: func(*config.Fact) (source.Source, error) { return nil, nil },
		Outputs:   outs,
		Actions:   h.actions,
		StatePath: statePath,
		BootID:    "boot-1",
	})
	if err := h.e.Restore(); err != nil {
		t.Fatal(err)
	}
	h.now = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	return h
}

// feed delivers a source value and evaluates.
func (h *harness) feed(fact string, v any) {
	h.t.Helper()
	h.e.onUpdate(source.Update{Fact: fact, Value: v})
	h.eval()
}

func (h *harness) eval() { h.e.Evaluate(h.now) }

func (h *harness) advance(d time.Duration) {
	h.now = h.now.Add(d)
	h.eval()
}

// offer sets a value at the harness clock (onUpdate uses the real clock).
func (h *harness) set(fact string, v any) {
	h.t.Helper()
	f := h.e.facts[fact]
	conv, err := h.e.convert(f, v)
	if err != nil {
		h.t.Fatal(err)
	}
	f.Err = ""
	h.e.offer(f, conv, h.now)
	h.eval()
}

func (h *harness) matched(rule string) bool {
	for _, r := range h.e.allRules() {
		if r.Def.Name == rule {
			return r.Matched
		}
	}
	h.t.Fatalf("no rule %s", rule)
	return false
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

func TestMotivatingCase(t *testing.T) {
	h := newHarness(t, sleepConfig, "")
	h.set("ssid", "HomeWifi")
	h.set("ac", true)
	if got := h.heldLock(); got != "handle-lid-switch:sleep" {
		t.Fatalf("at home on mains: lock %q", got)
	}
	if !h.matched("home-on-mains") || !h.matched("mains-elsewhere") {
		t.Fatal("expected both mains rules to match")
	}

	// Unplug: lid stays protected for 30 minutes.
	h.advance(time.Hour)
	h.set("ac", false)
	if got := h.heldLock(); got != "handle-lid-switch" {
		t.Fatalf("just unplugged: lock %q", got)
	}
	if len(h.actions.ran) != 1 || h.actions.ran[0] != "just-unplugged:enter:notify" {
		t.Fatalf("actions %v", h.actions.ran)
	}
	// The engine schedules its own wake-up at the threshold.
	wantWake := h.now.Add(30*time.Minute + time.Millisecond)
	if !h.e.nextWake.Equal(wantWake) {
		t.Fatalf("next wake %v, want %v", h.e.nextWake, wantWake)
	}
	h.advance(29 * time.Minute)
	if h.heldLock() != "handle-lid-switch" {
		t.Fatal("protection ended early")
	}
	h.now = h.e.nextWake
	h.eval()
	if got := h.heldLock(); got != "" {
		t.Fatalf("after 30 minutes on battery: lock %q", got)
	}
	if !h.e.nextWake.IsZero() {
		t.Fatalf("nothing left to schedule, but next wake is %v", h.e.nextWake)
	}

	// Plugged in away from home: only idle sleep is blocked.
	h.set("ssid", "Cafe")
	h.set("ac", true)
	if got := h.heldLock(); got != "sleep" {
		t.Fatalf("on mains elsewhere: lock %q", got)
	}
}

func TestNoGapWhenLocksChange(t *testing.T) {
	h := newHarness(t, sleepConfig, "")
	h.set("ssid", "HomeWifi")
	h.set("ac", true)
	h.advance(time.Minute)
	h.set("ac", false)
	// Two locks were taken in order; the second replaced the first.
	if got := strings.Join(h.locker.taken, " "); got != "handle-lid-switch:sleep handle-lid-switch" {
		t.Fatalf("taken %q", got)
	}
}

func TestFirstReadIsNotAChange(t *testing.T) {
	h := newHarness(t, sleepConfig, "")
	// Starting on battery must not count as "just unplugged".
	h.set("ssid", "x")
	h.set("ac", false)
	if h.matched("just-unplugged") {
		t.Fatal("first observation counted as a change")
	}
}

func TestFailedLockKeepsOldAndRetries(t *testing.T) {
	h := newHarness(t, sleepConfig, "")
	h.set("ssid", "HomeWifi")
	h.set("ac", true)
	h.locker.fail = true
	h.advance(time.Minute)
	h.set("ac", false)
	if got := h.heldLock(); got != "handle-lid-switch:sleep" {
		t.Fatalf("old lock should be kept when the new one fails, got %q", got)
	}
	if !h.e.outputErr || h.e.nextWake.Sub(h.now) != retryAfter {
		t.Fatalf("expected a retry in %s, next wake %v", retryAfter, h.e.nextWake.Sub(h.now))
	}
	h.locker.fail = false
	h.advance(retryAfter)
	if got := h.heldLock(); got != "handle-lid-switch" {
		t.Fatalf("after retry: %q", got)
	}
}

const carryConfig = `
[fact.ac]
type = "bool"
command = "true"
[fact.lid]
type = "bool"
command = "true"
[fact.carry]
state = ["off", "protecting"]

[[rule]]
name  = "unplugged"
while = "!ac"
enter = [{ set = { carry = "protecting" } }]

[[rule]]
name  = "arrived"
while = "carry == 'protecting' && (!lid || ac)"
enter = [{ set = { carry = "off" } }]

[[rule]]
name  = "timeout"
while = "carry == 'protecting' && since('carry') > duration('30m')"
enter = [{ set = { carry = "off" } }]

[[rule]]
name    = "protect"
while   = "carry == 'protecting'"
inhibit = ["handle-lid-switch"]
`

func TestStateMachine(t *testing.T) {
	h := newHarness(t, carryConfig, "")
	h.set("lid", true)
	h.set("ac", true)
	if h.heldLock() != "" {
		t.Fatal("nothing should be held on mains")
	}
	h.advance(time.Minute)
	h.set("ac", false)
	if got := h.e.facts["carry"].Value; got != "protecting" {
		t.Fatalf("carry = %v", got)
	}
	if h.heldLock() != "handle-lid-switch" {
		t.Fatal("lid should be protected")
	}
	// Opening the lid ends the protection early.
	h.advance(5 * time.Minute)
	h.set("lid", false)
	if got := h.e.facts["carry"].Value; got != "off" {
		t.Fatalf("carry = %v after opening the lid", got)
	}
	if h.heldLock() != "" {
		t.Fatal("protection should have ended")
	}
	// Closing it again does not bring it back: that history is in the state.
	h.advance(time.Minute)
	h.set("lid", true)
	if h.heldLock() != "" {
		t.Fatal("closing the lid again should not protect it")
	}

	// A fresh unplug with the lid closed times out after 30 minutes.
	h.set("ac", true)
	h.advance(time.Minute)
	h.set("ac", false)
	if h.heldLock() != "handle-lid-switch" {
		t.Fatal("second unplug should protect")
	}
	h.now = h.e.nextWake
	h.eval()
	if h.heldLock() != "" {
		t.Fatalf("timeout should end protection; carry=%v", h.e.facts["carry"].Value)
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
	h := newHarness(t, src, "")
	h.systemd.active["already.service"] = true
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

func TestDebounce(t *testing.T) {
	src := `
[fact.ssid]
type = "string"
command = "true"
debounce = "10s"
[[rule]]
name    = "home"
while   = "ssid == 'home'"
inhibit = ["idle"]
`
	h := newHarness(t, src, "")
	h.set("ssid", "home")
	if h.heldLock() != "idle" {
		t.Fatal("first value should apply at once")
	}
	h.advance(time.Minute)
	h.set("ssid", "")
	if h.heldLock() != "idle" {
		t.Fatal("a blip should not apply before the debounce")
	}
	h.advance(3 * time.Second)
	h.set("ssid", "home")
	h.advance(20 * time.Second)
	if h.heldLock() != "idle" || h.e.facts["ssid"].Value != "home" {
		t.Fatal("a blip that recovered should be ignored")
	}
	h.set("ssid", "")
	h.advance(10 * time.Second)
	if h.heldLock() != "" {
		t.Fatal("a lasting change should apply after the debounce")
	}
}

func TestTemporaryRules(t *testing.T) {
	h := newHarness(t, sleepConfig, "")
	h.set("ssid", "x")
	h.set("ac", false)
	h.e.started = h.now
	id, err := h.e.Ensure(TempSpec{Outputs: map[string]any{"inhibit": []any{"sleep"}}, Until: time.Now().Add(time.Hour)}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Ensure uses the real clock for Until; evaluate at real time.
	h.now = time.Now().Round(0)
	h.eval()
	if h.heldLock() != "sleep" {
		t.Fatalf("temporary rule %s should hold sleep", id)
	}
	if h.e.nextWake.After(h.now.Add(time.Hour)) || h.e.nextWake.IsZero() {
		t.Fatalf("next wake should be at the deadline, got %v", h.e.nextWake)
	}
	h.advance(time.Hour + time.Second)
	if h.heldLock() != "" {
		t.Fatal("temporary rule should have expired")
	}
	if len(h.e.temps) != 0 {
		t.Fatal("expired temporary rule should be dropped")
	}

	if _, err := h.e.Ensure(TempSpec{Outputs: map[string]any{"inhibit": []any{"idle"}}}, ""); err != nil {
		t.Fatal(err)
	}
	h.eval()
	if h.heldLock() != "idle" {
		t.Fatal("indefinite temporary rule should hold")
	}
	if n := h.e.Release(""); n != 1 {
		t.Fatalf("released %d", n)
	}
	h.eval()
	if h.heldLock() != "" || len(h.e.temps) != 0 {
		t.Fatal("released rule should be gone")
	}

	if _, err := h.e.Ensure(TempSpec{Outputs: map[string]any{"bogus": []any{"x"}}}, ""); err == nil {
		t.Fatal("unknown output should fail")
	}
	if _, err := h.e.Ensure(TempSpec{While: "nosuchfact", Outputs: map[string]any{"inhibit": []any{"idle"}}}, ""); err == nil {
		t.Fatal("bad condition should fail")
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	h := newHarness(t, sleepConfig+carryStateFact, path)
	h.set("ssid", "x")
	h.set("ac", true)
	h.advance(time.Minute)
	h.set("ac", false)
	if !h.matched("just-unplugged") {
		t.Fatal("setup: rule should match")
	}
	h.e.facts["mode"].Value = "away"
	h.e.save(true)
	ranBefore := len(h.actions.ran)

	// Same boot: since() continues and the rule is not entered again.
	h2 := newHarness(t, sleepConfig+carryStateFact, path)
	h2.now = h.now.Add(time.Minute)
	h2.set("ac", false)
	h2.set("ssid", "x")
	if !h2.matched("just-unplugged") {
		t.Fatal("since() should survive a restart in the same boot")
	}
	if len(h2.actions.ran) != 0 {
		t.Fatalf("enter actions re-ran after restart: %v (before: %d)", h2.actions.ran, ranBefore)
	}
	if h2.e.facts["mode"].Value != "away" {
		t.Fatal("state fact should be restored")
	}

	// New boot: old observations are only provisional, so being on battery
	// after a reboot is not "just unplugged".
	h3 := newHarness(t, sleepConfig+carryStateFact, path)
	h3.e.deps.BootID = "boot-2"
	if err := h3.e.Restore(); err != nil {
		t.Fatal(err)
	}
	h3.now = h.now.Add(time.Minute)
	h3.set("ac", false)
	h3.set("ssid", "x")
	if !h3.e.facts["ac"].Changed.IsZero() {
		t.Fatal("after a reboot, the first read is not a change")
	}
	if h3.e.facts["mode"].Value != "away" {
		t.Fatal("state facts survive reboots")
	}
}

const carryStateFact = `
[fact.mode]
state = ["home", "away"]
`

func TestCompileErrors(t *testing.T) {
	outs := []output.Output{output.NewInhibit(nil), output.NewUnits(nil)}
	for _, src := range []string{
		"[fact.a]\nexpr = \"b\"\n[fact.b]\nexpr = \"a\"\n",                                           // cycle
		"[[rule]]\nwhile = \"true\"\ninhbit = [\"sleep\"]\n",                                         // typo in output
		"[[rule]]\nwhile = \"true\"\ninhibit = [\"slep\"]\n",                                         // bad lock type
		"[[rule]]\nwhile = \"nope\"\n",                                                               // unknown fact
		"[[rule]]\nwhile = \"true\"\nunits = [\"foo\"]\n",                                            // unit without suffix
		"[fact.x]\ncommand = \"true\"\ntransform = \"value +\"\n",                                    // bad transform
		"[fact.x]\nstate = [\"a\"]\n[[rule]]\nwhile = \"true\"\nenter = [{ set = { x = \"b\" } }]\n", // invalid state
	} {
		cfg, err := config.Parse(src)
		if err != nil {
			continue
		}
		if _, err := Compile(cfg, outs); err == nil {
			t.Errorf("expected an error for:\n%s", src)
		}
	}
}

func TestStatusSnapshot(t *testing.T) {
	h := newHarness(t, sleepConfig, "")
	h.set("ssid", "HomeWifi")
	h.set("ac", true)
	st := h.e.Snapshot()
	var names []string
	for _, f := range st.Facts {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "ac,home,ssid" {
		t.Fatalf("facts %v", names)
	}
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
