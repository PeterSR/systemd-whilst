package bus

import (
	"testing"

	"github.com/godbus/dbus/v5"

	"github.com/PeterSR/whilst/value"
)

func TestValue(t *testing.T) {
	cases := []struct {
		in   any
		want any
	}{
		{uint32(4), int64(4)},
		{dbus.MakeVariant(true), true},
		{dbus.ObjectPath("/a"), "/a"},
		{dbus.SignatureOf(""), "s"},
		{[]string{"a", "b"}, []any{"a", "b"}},
		{map[string]dbus.Variant{"k": dbus.MakeVariant(int32(1))}, map[string]any{"k": int64(1)}},
		{[]dbus.ObjectPath{"/a", "/b"}, []any{"/a", "/b"}},
		{dbus.MakeVariant(dbus.MakeVariant(uint8(3))), int64(3)},
		{uint64(1 << 63), uint64(1 << 63)},
	}
	for _, c := range cases {
		if got := Value(c.in); !value.Equal(got, c.want) {
			t.Errorf("Value(%#v) = %#v, want %#v", c.in, got, c.want)
		}
	}
}
