// Package value normalises values from D-Bus, commands and TOML into a small
// set of Go types that CEL understands and that can be compared and persisted.
//
// The normal forms are: nil, bool, int64, uint64 (only when it does not fit in
// int64), float64, string, []any and map[string]any.
package value

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// Normalize converts v into its normal form.
func Normalize(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case dbus.Variant:
		return Normalize(x.Value())
	case bool:
		return x
	case string:
		return x
	case dbus.ObjectPath:
		return string(x)
	case dbus.Signature:
		return x.String()
	case int:
		return int64(x)
	case int8:
		return int64(x)
	case int16:
		return int64(x)
	case int32:
		return int64(x)
	case int64:
		return x
	case uint:
		return normUint(uint64(x))
	case uint8:
		return int64(x)
	case uint16:
		return int64(x)
	case uint32:
		return int64(x)
	case uint64:
		return normUint(x)
	case float32:
		return finite(float64(x))
	case float64:
		return finite(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Normalize(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = Normalize(e)
		}
		return out
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = Normalize(rv.Index(i).Interface())
		}
		return out
	case reflect.Map:
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out[keyString(Normalize(iter.Key().Interface()))] = Normalize(iter.Value().Interface())
		}
		return out
	case reflect.Pointer:
		if rv.IsNil() {
			return nil
		}
		return Normalize(rv.Elem().Interface())
	}
	return fmt.Sprint(v)
}

// finite maps NaN and infinities to nil: they cannot be stored as JSON and
// NaN is not even equal to itself, so it would look like a change on every
// read.
func finite(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return f
}

func normUint(u uint64) any {
	if u <= math.MaxInt64 {
		return int64(u)
	}
	return u
}

func keyString(k any) string {
	if s, ok := k.(string); ok {
		return s
	}
	return fmt.Sprint(k)
}

// Equal reports whether two normalised values are equal. Numbers compare by
// value, so 80 and 80.0 are equal: a double survives a round trip through
// JSON (the state file) as an int when it is whole.
func Equal(a, b any) bool {
	switch x := a.(type) {
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !Equal(v, w) {
				return false
			}
		}
		return true
	}
	if fa, ok := number(a); ok {
		if fb, ok := number(b); ok {
			return numEqual(a, b, fa, fb)
		}
		return false
	}
	return reflect.DeepEqual(a, b)
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func numEqual(a, b any, fa, fb float64) bool {
	// Exact integer comparison where both are integers, so large values
	// that round to the same float stay distinct.
	ia, aInt := a.(int64)
	ib, bInt := b.(int64)
	if aInt && bInt {
		return ia == ib
	}
	ua, aU := a.(uint64)
	ub, bU := b.(uint64)
	if aU && bU {
		return ua == ub
	}
	return fa == fb
}

// Format renders a normalised value for humans.
func Format(v any) string {
	switch x := v.(type) {
	case nil:
		return "(none)"
	case string:
		return strconv.Quote(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = Format(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = strconv.Quote(k) + ": " + Format(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// FromJSON re-normalises a value decoded from JSON (where every number is a
// float64 or json.Number) so it compares equal to the value it was encoded
// from. Whole numbers become int64.
func FromJSON(v any) any {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x)
		}
		return x
	case []any:
		for i := range x {
			x[i] = FromJSON(x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = FromJSON(x[k])
		}
		return x
	}
	return Normalize(v)
}

// Kind names the type of a normalised value, matching the names used for the
// `type` key in config.
func Kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case int64:
		return "int"
	case uint64:
		return "uint"
	case float64:
		return "double"
	case string:
		return "string"
	case []any:
		return "list"
	case map[string]any:
		return "map"
	}
	return "unknown"
}

// Coerce converts v to the declared kind where that is lossless or obvious
// (for example "1" to bool true for a sysfs file). An empty kind or "dyn"
// returns v unchanged.
func Coerce(v any, kind string) (any, error) {
	if kind == "" || kind == "dyn" || Kind(v) == kind {
		return v, nil
	}
	switch kind {
	case "bool":
		switch x := v.(type) {
		case int64:
			return x != 0, nil
		case string:
			s := strings.ToLower(strings.TrimSpace(x))
			switch s {
			case "1", "true", "yes", "on":
				return true, nil
			case "0", "false", "no", "off", "":
				return false, nil
			}
		}
	case "int":
		switch x := v.(type) {
		case string:
			if i, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
				return i, nil
			}
		case float64:
			if x == math.Trunc(x) {
				return int64(x), nil
			}
		case bool:
			if x {
				return int64(1), nil
			}
			return int64(0), nil
		}
	case "double":
		switch x := v.(type) {
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
				return f, nil
			}
		case int64:
			return float64(x), nil
		case uint64:
			return float64(x), nil
		}
	case "string":
		switch x := v.(type) {
		case bool, int64, uint64, float64:
			return fmt.Sprint(x), nil
		}
	case "uint":
		switch x := v.(type) {
		case int64:
			if x >= 0 {
				return uint64(x), nil
			}
		case string:
			if u, err := strconv.ParseUint(strings.TrimSpace(x), 10, 64); err == nil {
				return u, nil
			}
		}
	}
	return nil, fmt.Errorf("cannot use %s value %s as %s", Kind(v), Format(v), kind)
}

// Kinds lists the names accepted for a fact's `type`.
var Kinds = []string{"dyn", "bool", "int", "uint", "double", "string", "list", "map"}
