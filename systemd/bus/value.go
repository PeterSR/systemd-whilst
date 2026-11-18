package bus

import (
	"github.com/godbus/dbus/v5"

	"github.com/PeterSR/whilst/value"
)

// Value converts a value read from D-Bus (or anywhere else) into the normal
// form of package value. Variants are unwrapped, and object paths and
// signatures become strings, at any depth.
func Value(v any) any { return value.NormalizeWith(v, plain) }

func plain(v any) (any, bool) {
	switch x := v.(type) {
	case dbus.Variant:
		return x.Value(), true
	case dbus.ObjectPath:
		return string(x), true
	case dbus.Signature:
		return x.String(), true
	}
	return nil, false
}
