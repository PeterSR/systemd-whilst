package value

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		in   any
		want any
	}{
		{uint32(4), int64(4)},
		{dbus.MakeVariant(true), true},
		{dbus.ObjectPath("/a"), "/a"},
		{[]string{"a", "b"}, []any{"a", "b"}},
		{map[string]dbus.Variant{"k": dbus.MakeVariant(int32(1))}, map[string]any{"k": int64(1)}},
		{uint64(1 << 63), uint64(1 << 63)},
	}
	for _, c := range cases {
		if got := Normalize(c.in); !Equal(got, c.want) {
			t.Errorf("Normalize(%#v) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestCoerce(t *testing.T) {
	ok := []struct {
		in   any
		kind string
		want any
	}{
		{"1", "bool", true},
		{"0", "bool", false},
		{"42", "int", int64(42)},
		{"96.5", "double", 96.5},
		{int64(3), "double", 3.0},
		{true, "string", "true"},
		{int64(5), "dyn", int64(5)},
	}
	for _, c := range ok {
		got, err := Coerce(c.in, c.kind)
		if err != nil || !Equal(got, c.want) {
			t.Errorf("Coerce(%#v, %s) = %#v, %v", c.in, c.kind, got, err)
		}
	}
	if _, err := Coerce("maybe", "bool"); err == nil {
		t.Error("expected an error")
	}
	if _, err := Coerce("x", "list"); err == nil {
		t.Error("expected an error")
	}
}

func TestFromJSON(t *testing.T) {
	if got := FromJSON(float64(3)); got != int64(3) {
		t.Errorf("%#v", got)
	}
	if got := FromJSON(2.5); got != 2.5 {
		t.Errorf("%#v", got)
	}
}

func TestEqualNumbers(t *testing.T) {
	if !Equal(int64(80), 80.0) || !Equal([]any{80.0}, []any{int64(80)}) || !Equal(map[string]any{"a": 1.0}, map[string]any{"a": int64(1)}) {
		t.Error("whole doubles should equal ints")
	}
	if Equal(int64(80), 80.5) || Equal(int64(1), "1") || Equal(int64(1), true) {
		t.Error("unequal values compared equal")
	}
	if Equal(int64(1<<62), int64(1<<62+1)) {
		t.Error("large ints must compare exactly")
	}
}
