package bus

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestMatchRoundTrip(t *testing.T) {
	src := "type='signal',sender='org.freedesktop.UPower',path='/org/freedesktop/UPower',interface='org.freedesktop.DBus.Properties',member='PropertiesChanged',arg0='org.freedesktop.UPower'"
	m, err := ParseMatch(src)
	if err != nil {
		t.Fatal(err)
	}
	if m.String() != src {
		t.Fatalf("round trip:\n%s\n%s", m.String(), src)
	}
	q, err := ParseMatch(`member='X',arg0='it'\''s'`)
	if err != nil || q.Args[0] != "it's" {
		t.Fatalf("quoting: %+v %v", q, err)
	}
	if _, err := ParseMatch("type='method_call'"); err == nil {
		t.Fatal("only signals are supported")
	}
	if _, err := ParseMatch("bogus='x'"); err == nil {
		t.Fatal("unknown key should fail")
	}
}

func TestMatches(t *testing.T) {
	m := Match{Sender: "org.freedesktop.UPower", Path: "/org/freedesktop/UPower", Interface: "org.freedesktop.DBus.Properties", Member: "PropertiesChanged", Args: map[int]string{0: "org.freedesktop.UPower"}}
	sig := &dbus.Signal{Sender: ":1.5", Path: "/org/freedesktop/UPower", Name: "org.freedesktop.DBus.Properties.PropertiesChanged", Body: []any{"org.freedesktop.UPower", map[string]dbus.Variant{}, []string{}}}
	owners := map[string]string{"org.freedesktop.UPower": ":1.5"}
	if !m.matches(sig, owners) {
		t.Fatal("should match via owner")
	}
	if m.matches(sig, map[string]string{"org.freedesktop.UPower": ":1.9"}) {
		t.Fatal("wrong owner should not match")
	}
	sig.Body[0] = "other"
	if m.matches(sig, owners) {
		t.Fatal("arg0 mismatch should not match")
	}
	ns := Match{PathNamespace: "/org/freedesktop/NetworkManager"}
	if !ns.matches(&dbus.Signal{Path: "/org/freedesktop/NetworkManager/Devices/1", Name: "a.B"}, nil) {
		t.Fatal("path_namespace should match children")
	}
	if ns.matches(&dbus.Signal{Path: "/org/freedesktop/NetworkManagerX", Name: "a.B"}, nil) {
		t.Fatal("path_namespace should not match siblings with the same prefix")
	}
}

func TestMatchValidation(t *testing.T) {
	for _, bad := range []string{"interface='nodots'", "member='1x'", "path='no/slash'", "sender='a'"} {
		if _, err := ParseMatch(bad); err == nil {
			t.Errorf("%s: expected an error", bad)
		}
	}
	if _, err := ParseMatch("sender=':1.42',path_namespace='/org/a'"); err != nil {
		t.Error(err)
	}
}
