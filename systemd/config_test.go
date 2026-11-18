package systemd_test

// Tests for the fact and action kinds whilst registers with the core config.

import (
	"strings"
	"testing"
	"time"

	"github.com/PeterSR/systemd-whilst/systemd"
	"github.com/PeterSR/systemd-whilst/systemd/action"
	"github.com/PeterSR/systemd-whilst/systemd/source"
	"github.com/PeterSR/whilst/config"
)

func TestParseFull(t *testing.T) {
	c, err := config.Parse(`
include = ["preset:upower", "preset:logind"]
tick = "1m"

[fact.ssid]
command = "echo hi"
on_dbus = ["type='signal',sender='org.freedesktop.NetworkManager',member='StateChanged'"]
every   = "1m"
debounce = "5s"

[fact.home]
expr = "ssid == 'x'"

[fact.mode]
state = ["off", "on"]

[fact.sig]
dbus_signal = { member = "PrepareForSleep", interface = "org.freedesktop.login1.Manager" }
arg = 0

[[rule]]
name    = "r"
while   = "home && ac"
inhibit = ["sleep"]
enter   = [{ notify = "hi" }, { set = { mode = "on" } }, { command = "true", timeout = "5s" }]
exit    = [{ dbus_call = { service = "org.freedesktop.login1", path = "/org/freedesktop/login1", method = "org.freedesktop.login1.Manager.Suspend", args = [false], signature = "b" } }]
`, systemd.Registry())
	if err == nil {
		t.Fatal("arg outside the dbus_signal table should be rejected")
	}
	c, err = config.Parse(`
include = ["preset:upower", "preset:logind"]
tick = "1m"

[fact.ssid]
command = "echo hi"
on_dbus = ["type='signal',sender='org.freedesktop.NetworkManager',member='StateChanged'"]
every   = "1m"
debounce = "5s"

[fact.home]
expr = "ssid == 'x'"

[fact.mode]
state = ["off", "on"]

[fact.sig]
dbus_signal = { member = "PrepareForSleep", interface = "org.freedesktop.login1.Manager", arg = 0 }

[[rule]]
name    = "r"
while   = "home && ac"
inhibit = ["sleep"]
enter   = [{ notify = "hi" }, { set = { mode = "on" } }, { command = "true", timeout = "5s" }]
exit    = [{ dbus_call = { service = "org.freedesktop.login1", path = "/org/freedesktop/login1", method = "org.freedesktop.login1.Manager.Suspend", args = [false], signature = "b" } }]
`, systemd.Registry())
	if err != nil {
		t.Fatal(err)
	}
	if c.Tick != time.Minute {
		t.Errorf("tick %v", c.Tick)
	}
	for _, n := range []string{"ac", "battery", "lid", "docked", "ssid", "home", "mode", "sig"} {
		if c.Facts[n] == nil {
			t.Errorf("missing fact %s", n)
		}
	}
	if c.Facts["mode"].Initial != "off" {
		t.Errorf("state initial = %v", c.Facts["mode"].Initial)
	}
	if sig := c.Facts["sig"].Spec.(*source.Spec).DBusSignal; sig.Arg != 0 {
		t.Errorf("signal arg %d", sig.Arg)
	}
	r := c.Rules[0]
	if len(r.Enter) != 3 || len(r.Exit) != 1 || r.Outputs["inhibit"] == nil {
		t.Errorf("rule %+v", r)
	}
	if d := r.Exit[0].Spec.(*action.DBusCall); d.Interface != "org.freedesktop.login1.Manager" || d.Method != "Suspend" || d.Bus != "system" {
		t.Errorf("dbus_call %+v", d)
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{
		`bogus = 1`,
		"[fact.x]\n", // no source
		"[fact.x]\ncommand = \"a\"\nexpr = \"b\"\n",                                            // two sources
		"[fact.x]\ncommand = \"a\"\nevry = \"1m\"\n",                                           // typo
		"[fact.now]\ncommand = \"a\"\n",                                                        // reserved
		"[fact.x]\nexpr = \"a\"\nevery = \"1m\"\n",                                             // every on expr
		"[fact.x]\nstate = true\n",                                                             // free state without initial
		"[fact.x]\nstate = [\"a\"]\ninitial = \"b\"\n",                                         // initial not allowed
		"[[rule]]\ninhibit = [\"sleep\"]\n",                                                    // no while
		"[[rule]]\nname = \"a\"\nwhile = \"true\"\n[[rule]]\nname = \"a\"\nwhile = \"true\"\n", // dup
		"[[rule]]\nwhile = \"true\"\nenter = [{ notify = \"a\", command = \"b\" }]\n",
		"[[rule]]\nwhile = \"true\"\nenter = [{ set = { nofact = 1 } }]\n",
		"[[rule]]\nwhile = \"true\"\nexit = [{ dbus_call = { service = \"a\", path = \"/\", method = \"x.Y\", args = [300], signature = \"y\" } }]\n",
		"include = [\"preset:nope\"]\n",
		"[fact.x]\nunit = \"nosuffix\"\n",
		"[fact.x]\ncommand = \"a\"\non_dbus = [\"type='method_call'\"]\n",
		"[fact.x]\ncommand = \"a\"\non_dbus = [\"path='/a',path_namespace='/b'\"]\n", // path and path_namespace together
		"[fact.x]\ndbus_signal = { member = \"X\" }\nevery = \"1m\"\n",               // no triggers on a signal
	} {
		if _, err := config.Parse(src, systemd.Registry()); err == nil {
			t.Errorf("expected an error for:\n%s", src)
		}
	}
}

// goldenConfig has a fact of every kind and unnamed rules with every action
// kind. The expected names and source keys below were produced by whilst
// before the core and the application were split; they are stored in state
// files, so they must not change.
const goldenConfig = `
[fact.p]
dbus = { service = "org.freedesktop.UPower", path = "/org/freedesktop/UPower", interface = "org.freedesktop.UPower", property = "OnBattery" }
on_dbus = ["type='signal',sender='org.freedesktop.NetworkManager',member='StateChanged'"]
every = "10s"
transform = "!value"
type = "bool"
[fact.sig]
dbus_signal = { member = "PrepareForSleep", interface = "org.freedesktop.login1.Manager", arg = 0, bus = "system" }
initial = false
[fact.sig2]
dbus_signal = { match = "type='signal',interface='a.b',member='C'" }
[fact.u1]
unit = "foo.service"
[fact.u2]
unit = { name = "bar.timer", bus = "system" }
[fact.c]
command = "echo hi"
output = "json"
timeout = "3s"
on_dbus_session = ["type='signal',member='X'"]
[fact.c2]
command = "true"
[fact.f]
file = "/sys/class/power_supply/AC/online"
type = "int"
debounce = "2s"
[fact.e]
expr = "c2 == 'x'"
[fact.s]
state = ["off", "on"]
[fact.s2]
state = true
initial = 3

[[rule]]
while = "true"
inhibit = ["sleep", "idle"]
enter = [{ notify = "hi" }, { notify = { summary = "s", body = "b", urgency = "low" } }]
[[rule]]
while = "e"
units = ["a.service"]
exit = [{ command = "echo x", timeout = "5s" }, { command = "true" }]
[[rule]]
while = "p"
hold = "!p"
exit = [{ dbus_call = { service = "org.freedesktop.login1", path = "/org/freedesktop/login1", method = "org.freedesktop.login1.Manager.Suspend", args = [false], signature = "b" } }]
[[rule]]
while = "p"
enter = [{ set = { s = "on", s2 = 4 } }, { dbus_call = { service = "a.b", path = "/a", interface = "a.b", method = "M", args = ["x", 1, 2.5, true] } }]
[[rule]]
while = "p"
inhibit = "sleep"
enter = []
[[rule]]
while = "p"
inhibit = "sleep"
[[rule]]
while = "p"
inhibit = "sleep"
`

func TestGoldenNamesAndSourceKeys(t *testing.T) {
	c, err := config.Parse(goldenConfig, systemd.Registry())
	if err != nil {
		t.Fatal(err)
	}
	wantRules := []string{"rule-b1d9c55e", "rule-91e4d4bd", "rule-e01cfa71", "rule-ca80ae7d", "rule-9ebec054", "rule-f642e73a", "rule-f642e73a-2"}
	if len(c.Rules) != len(wantRules) {
		t.Fatalf("%d rules", len(c.Rules))
	}
	for i, r := range c.Rules {
		if r.Name != wantRules[i] {
			t.Errorf("rule %d: name %s, want %s", i+1, r.Name, wantRules[i])
		}
	}
	// The engine stores SourceKey|type|transform.
	wantKeys := map[string]string{
		"c":    "[\"command\",null,null,null,{\"Run\":\"echo hi\",\"Output\":\"json\",\"Timeout\":0},\"\",\"\",null]||",
		"c2":   "[\"command\",null,null,null,{\"Run\":\"true\",\"Output\":\"stdout\",\"Timeout\":0},\"\",\"\",null]||",
		"e":    "[\"expr\",null,null,null,null,\"\",\"c2 == 'x'\",null]||",
		"f":    "[\"file\",null,null,null,null,\"/sys/class/power_supply/AC/online\",\"\",null]|int|",
		"p":    "[\"dbus\",{\"Bus\":\"system\",\"Service\":\"org.freedesktop.UPower\",\"Path\":\"/org/freedesktop/UPower\",\"Interface\":\"org.freedesktop.UPower\",\"Property\":\"OnBattery\"},null,null,null,\"\",\"\",null]|bool|!value",
		"s":    "[\"state\",null,null,null,null,\"\",\"\",[\"off\",\"on\"]]|string|",
		"s2":   "[\"state\",null,null,null,null,\"\",\"\",null]||",
		"sig":  "[\"dbus_signal\",null,{\"Bus\":\"system\",\"Match\":{\"Type\":\"signal\",\"Sender\":\"\",\"Path\":\"\",\"PathNamespace\":\"\",\"Interface\":\"org.freedesktop.login1.Manager\",\"Member\":\"PrepareForSleep\",\"Args\":null},\"Arg\":0},null,null,\"\",\"\",null]||",
		"sig2": "[\"dbus_signal\",null,{\"Bus\":\"system\",\"Match\":{\"Type\":\"signal\",\"Sender\":\"\",\"Path\":\"\",\"PathNamespace\":\"\",\"Interface\":\"a.b\",\"Member\":\"C\",\"Args\":null},\"Arg\":-1},null,null,\"\",\"\",null]||",
		"u1":   "[\"unit\",null,null,{\"Bus\":\"session\",\"Name\":\"foo.service\"},null,\"\",\"\",null]||",
		"u2":   "[\"unit\",null,null,{\"Bus\":\"system\",\"Name\":\"bar.timer\"},null,\"\",\"\",null]||",
	}
	for name, want := range wantKeys {
		f := c.Facts[name]
		if got := f.SourceKey() + "|" + f.Type + "|" + f.Transform; got != want {
			t.Errorf("fact %s: key\n%s, want\n%s", name, got, want)
		}
	}
	for name, want := range map[string]bool{"p": true, "sig": false, "u1": true, "c": true, "f": true, "e": true, "s": false} {
		if got := c.Facts[name].Rereadable(); got != want {
			t.Errorf("fact %s: rereadable %v", name, got)
		}
	}
}

// A command's timeout does not change its value, so it is not part of the
// source key; its command line is.
func TestCommandTimeoutNotInSourceKey(t *testing.T) {
	a, _ := config.Parse("[fact.c]\ncommand = \"x\"\ntimeout = \"3s\"\n", systemd.Registry())
	b, _ := config.Parse("[fact.c]\ncommand = \"x\"\nevery = \"1m\"\n", systemd.Registry())
	d, _ := config.Parse("[fact.c]\ncommand = \"y\"\n", systemd.Registry())
	if a.Facts["c"].SourceKey() != b.Facts["c"].SourceKey() || a.Facts["c"].SourceKey() == d.Facts["c"].SourceKey() {
		t.Fatal("source key depends on the timeout or triggers, or not on the command")
	}
	if a.Facts["c"].Spec.(*source.Spec).Command.Timeout != 3*time.Second {
		t.Fatal("timeout lost")
	}
}

// Registering another action kind does not rename unnamed rules that do not
// use it, whether they have actions or not.
func TestNewActionKindKeepsNames(t *testing.T) {
	src := `
[fact.m]
state = ["a", "b"]
[[rule]]
while   = "m == 'a'"
inhibit = ["sleep"]
[[rule]]
while = "m == 'b'"
enter = [{ notify = "hi" }, { set = { m = "a" } }]
exit  = [{ command = "true", timeout = "5s" }]
`
	names := func(reg *config.Registry) []string {
		cfg, err := config.Parse(src, reg)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range cfg.Rules {
			out = append(out, r.Name)
		}
		return out
	}
	before := names(systemd.Registry())
	reg := systemd.Registry()
	reg.AddActionKind(config.ActionKind{Name: "beep", Parse: func(v any, _ map[string]any) (any, error) { return v, nil }})
	after := names(reg)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("names changed from %v to %v", before, after)
	}
	withBeep := strings.Replace(src, `[{ notify = "hi" }`, `[{ beep = 1 }, { notify = "hi" }`, 1)
	cfg, err := config.Parse(withBeep, reg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rules[1].Name == after[1] || cfg.Rules[0].Name != after[0] {
		t.Fatalf("using the new kind should rename only that rule: %s %s", cfg.Rules[0].Name, cfg.Rules[1].Name)
	}
}

// The core words these errors neutrally; whilst words them as v0.4.0 did
// (through its Registry), and the text must not change.
func TestAppWordedErrors(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"[[rule]]\nwhile = \"true\"\nenter = [\"x\"]\n", `(inline): rule 1: enter: each action must be a table, like { notify = "..." }`},
		{"[[rule]]\nname = \"ensure:x\"\nwhile = \"true\"\n", "rule ensure:x: names starting with ensure: are reserved for `whilst ensure`"},
	} {
		_, err := config.Parse(c.src, systemd.Registry())
		if err == nil || err.Error() != c.want {
			t.Errorf("error for:\n%s\ngot  %v\nwant %s", c.src, err, c.want)
		}
	}
}
