package config

import (
	"fmt"
	"math"

	"github.com/godbus/dbus/v5"
)

// ConvertArgs converts TOML values to the Go types godbus sends for the given
// signature. Only basic types are supported. With an empty signature the
// values are sent as they are: strings as s, integers as x, booleans as b,
// floats as d.
func ConvertArgs(signature string, args []any) ([]any, error) {
	if signature == "" {
		for _, a := range args {
			switch a.(type) {
			case string, int64, bool, float64:
			default:
				return nil, fmt.Errorf("argument %v needs a signature", a)
			}
		}
		return args, nil
	}
	if len(signature) != len(args) {
		return nil, fmt.Errorf("signature %q has %d types but there are %d args", signature, len(signature), len(args))
	}
	out := make([]any, len(args))
	for i, a := range args {
		c := signature[i]
		switch c {
		case 's', 'o', 'g':
			s, ok := a.(string)
			if !ok {
				return nil, fmt.Errorf("arg %d: %c needs a string", i+1, c)
			}
			switch c {
			case 's':
				out[i] = s
			case 'o':
				p := dbus.ObjectPath(s)
				if !p.IsValid() {
					return nil, fmt.Errorf("arg %d: %q is not a valid object path", i+1, s)
				}
				out[i] = p
			case 'g':
				sig, err := dbus.ParseSignature(s)
				if err != nil {
					return nil, fmt.Errorf("arg %d: %w", i+1, err)
				}
				out[i] = sig
			}
		case 'b':
			b, ok := a.(bool)
			if !ok {
				return nil, fmt.Errorf("arg %d: b needs true or false", i+1)
			}
			out[i] = b
		case 'd':
			switch x := a.(type) {
			case float64:
				out[i] = x
			case int64:
				out[i] = float64(x)
			default:
				return nil, fmt.Errorf("arg %d: d needs a number", i+1)
			}
		case 'y', 'n', 'q', 'i', 'u', 'x', 't':
			n, ok := a.(int64)
			if !ok {
				return nil, fmt.Errorf("arg %d: %c needs an integer", i+1, c)
			}
			var lo, hi float64
			switch c {
			case 'y':
				lo, hi = 0, math.MaxUint8
			case 'n':
				lo, hi = math.MinInt16, math.MaxInt16
			case 'q':
				lo, hi = 0, math.MaxUint16
			case 'i':
				lo, hi = math.MinInt32, math.MaxInt32
			case 'u':
				lo, hi = 0, math.MaxUint32
			case 'x':
				lo, hi = math.MinInt64, math.MaxInt64
			case 't':
				lo, hi = 0, math.MaxInt64
			}
			if float64(n) < lo || float64(n) > hi {
				return nil, fmt.Errorf("arg %d: %d is out of range for %c", i+1, n, c)
			}
			switch c {
			case 'y':
				out[i] = uint8(n)
			case 'n':
				out[i] = int16(n)
			case 'q':
				out[i] = uint16(n)
			case 'i':
				out[i] = int32(n)
			case 'u':
				out[i] = uint32(n)
			case 'x':
				out[i] = n
			case 't':
				out[i] = uint64(n)
			}
		default:
			return nil, fmt.Errorf("signature %q: only basic types (s o g b d y n q i u x t) are supported", signature)
		}
	}
	return out, nil
}
