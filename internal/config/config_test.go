package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseFull(t *testing.T) {
	c, err := Parse(`
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
`)
	if err == nil {
		t.Fatal("arg outside the dbus_signal table should be rejected")
	}
	c, err = Parse(`
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
`)
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
	if c.Facts["sig"].DBusSignal.Arg != 0 {
		t.Errorf("signal arg %d", c.Facts["sig"].DBusSignal.Arg)
	}
	r := c.Rules[0]
	if len(r.Enter) != 3 || len(r.Exit) != 1 || r.Outputs["inhibit"] == nil {
		t.Errorf("rule %+v", r)
	}
	if d := r.Exit[0].DBusCall; d.Interface != "org.freedesktop.login1.Manager" || d.Method != "Suspend" || d.Bus != "system" {
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
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("expected an error for:\n%s", src)
		}
	}
}

func TestIncludeFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "facts.toml"), []byte("[fact.x]\ncommand = \"true\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.toml"), []byte("include = [\"facts.toml\"]\n[[rule]]\nwhile = \"x == ''\"\n"), 0o644)
	c, err := Load(filepath.Join(dir, "main.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Facts["x"] == nil || !strings.HasSuffix(c.Facts["x"].Origin, "facts.toml") {
		t.Fatalf("include not loaded: %+v", c.Facts)
	}
	os.WriteFile(filepath.Join(dir, "loop.toml"), []byte("include = [\"loop.toml\"]\n"), 0o644)
	if _, err := Load(filepath.Join(dir, "loop.toml")); err == nil {
		t.Fatal("self-include should fail")
	}
	os.WriteFile(filepath.Join(dir, "tick.toml"), []byte("tick = \"1m\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main2.toml"), []byte("include = [\"tick.toml\"]\n"), 0o644)
	if _, err := Load(filepath.Join(dir, "main2.toml")); err == nil {
		t.Fatal("tick in an included file should fail")
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"2h": 2 * time.Hour, "1.5": 90 * time.Minute, "45m": 45 * time.Minute, "2": 2 * time.Hour} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if _, err := ParseDuration("soon"); err == nil {
		t.Error("expected error")
	}
}

func TestUnnamedRulesAreStable(t *testing.T) {
	a, err := Parse("[[rule]]\nwhile = \"true\"\n[[rule]]\nwhile = \"false\"\n[[rule]]\nwhile = \"false\"\n")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("[[rule]]\nwhile = \"false\"\n[[rule]]\nwhile = \"true\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if a.Rules[0].Name != b.Rules[1].Name || a.Rules[1].Name != b.Rules[0].Name {
		t.Fatalf("names depend on position: %s %s / %s %s", a.Rules[0].Name, a.Rules[1].Name, b.Rules[0].Name, b.Rules[1].Name)
	}
	if a.Rules[1].Name == a.Rules[2].Name || !strings.HasPrefix(a.Rules[2].Name, a.Rules[1].Name+"-") {
		t.Fatalf("identical rules: %s %s", a.Rules[1].Name, a.Rules[2].Name)
	}
}

func TestStateDoubleLiterals(t *testing.T) {
	if _, err := Parse("[fact.level]\ntype = \"double\"\nstate = true\ninitial = 1\n[[rule]]\nwhile = \"true\"\nenter = [{ set = { level = 2 } }]\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse("[fact.x]\ncommand = \"a\"\non_dbus = [\"path='/a',path_namespace='/b'\"]\n"); err == nil {
		t.Fatal("path and path_namespace together should fail")
	}
}

func TestLoadRecordsFiles(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "config.toml")
	inc := filepath.Join(dir, "more.toml")
	os.WriteFile(main, []byte("include = [\"preset:upower\", \"more.toml\"]\n"), 0o644)
	os.WriteFile(inc, []byte("[fact.x]\nstate = [\"a\"]\ninitial = \"a\"\n"), 0o644)
	cfg, err := Load(main)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Files) != 2 || cfg.Files[0] != main || cfg.Files[1] != inc {
		t.Fatalf("files %v, want the main file and the include (not the preset)", cfg.Files)
	}
}
