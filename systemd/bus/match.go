package bus

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
)

// Match is a D-Bus match rule, used both to ask the bus for signals and to
// route received signals to subscribers.
type Match struct {
	Type          string
	Sender        string
	Path          string
	PathNamespace string
	Interface     string
	Member        string
	Args          map[int]string
}

// ParseMatch parses a match rule string such as
// "type='signal',sender='org.freedesktop.UPower',member='PropertiesChanged'".
func ParseMatch(rule string) (Match, error) {
	var m Match
	rest := strings.TrimSpace(rule)
	for rest != "" {
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			return m, fmt.Errorf("match rule %q: expected key='value'", rule)
		}
		key := strings.TrimSpace(rest[:eq])
		rest = rest[eq+1:]
		val, n, err := parseMatchValue(rest)
		if err != nil {
			return m, fmt.Errorf("match rule %q: %w", rule, err)
		}
		rest = strings.TrimSpace(rest[n:])
		if strings.HasPrefix(rest, ",") {
			rest = strings.TrimSpace(rest[1:])
		} else if rest != "" {
			return m, fmt.Errorf("match rule %q: expected ',' after %s", rule, key)
		}
		switch {
		case key == "type":
			m.Type = val
		case key == "sender":
			m.Sender = val
		case key == "path":
			m.Path = val
		case key == "path_namespace":
			m.PathNamespace = val
		case key == "interface":
			m.Interface = val
		case key == "member":
			m.Member = val
		case strings.HasPrefix(key, "arg"):
			i, err := strconv.Atoi(key[3:])
			if err != nil || i < 0 || i > 63 {
				return m, fmt.Errorf("match rule %q: unsupported key %q", rule, key)
			}
			if m.Args == nil {
				m.Args = map[int]string{}
			}
			m.Args[i] = val
		default:
			return m, fmt.Errorf("match rule %q: unsupported key %q", rule, key)
		}
	}
	if m.Type != "" && m.Type != "signal" {
		return m, fmt.Errorf("match rule %q: only type='signal' is supported", rule)
	}
	if err := m.Validate(); err != nil {
		return m, fmt.Errorf("match rule %q: %w", rule, err)
	}
	return m, nil
}

var (
	elemRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	busElem  = regexp.MustCompile(`^[A-Za-z_-][A-Za-z0-9_-]*$`)
	uniqElem = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// ValidInterface checks D-Bus interface name syntax.
func ValidInterface(s string) error {
	parts := strings.Split(s, ".")
	if len(s) > 255 || len(parts) < 2 {
		return fmt.Errorf("%q is not a valid interface name (needs at least one dot)", s)
	}
	for _, p := range parts {
		if !elemRe.MatchString(p) {
			return fmt.Errorf("%q is not a valid interface name", s)
		}
	}
	return nil
}

// ValidMember checks D-Bus member (method, signal, property) name syntax.
func ValidMember(s string) error {
	if len(s) > 255 || !elemRe.MatchString(s) {
		return fmt.Errorf("%q is not a valid member name", s)
	}
	return nil
}

// ValidBusName checks a unique or well-known bus name.
func ValidBusName(s string) error {
	unique := strings.HasPrefix(s, ":")
	parts := strings.Split(strings.TrimPrefix(s, ":"), ".")
	if len(s) > 255 || len(parts) < 2 {
		return fmt.Errorf("%q is not a valid bus name", s)
	}
	for _, p := range parts {
		if (unique && !uniqElem.MatchString(p)) || (!unique && !busElem.MatchString(p)) {
			return fmt.Errorf("%q is not a valid bus name", s)
		}
	}
	return nil
}

// ValidPath checks object path syntax.
func ValidPath(s string) error {
	if !dbus.ObjectPath(s).IsValid() {
		return fmt.Errorf("%q is not a valid object path", s)
	}
	return nil
}

// Validate checks the syntax of every field, so mistakes surface at config
// load instead of when the bus rejects the rule.
func (m Match) Validate() error {
	if m.Path != "" && m.PathNamespace != "" {
		return fmt.Errorf("give path or path_namespace, not both")
	}
	if m.Sender != "" {
		if err := ValidBusName(m.Sender); err != nil {
			return err
		}
	}
	for _, p := range []string{m.Path, m.PathNamespace} {
		if p != "" {
			if err := ValidPath(p); err != nil {
				return err
			}
		}
	}
	if m.Interface != "" {
		if err := ValidInterface(m.Interface); err != nil {
			return err
		}
	}
	if m.Member != "" {
		if err := ValidMember(m.Member); err != nil {
			return err
		}
	}
	return nil
}

// parseMatchValue reads a value in D-Bus match rule quoting: text in single
// quotes, with \' outside quotes standing for a literal quote.
func parseMatchValue(s string) (string, int, error) {
	var b strings.Builder
	i := 0
	inQuote := false
	for i < len(s) {
		ch := s[i]
		switch {
		case ch == '\'':
			inQuote = !inQuote
			i++
		case !inQuote && ch == '\\' && i+1 < len(s) && s[i+1] == '\'':
			b.WriteByte('\'')
			i += 2
		case !inQuote && ch == ',':
			return b.String(), i, nil
		default:
			b.WriteByte(ch)
			i++
		}
	}
	if inQuote {
		return "", 0, fmt.Errorf("unterminated quote")
	}
	return b.String(), i, nil
}

func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// String renders the rule for AddMatch.
func (m Match) String() string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+quote(v))
		}
	}
	add("type", m.Type)
	add("sender", m.Sender)
	add("path", m.Path)
	add("path_namespace", m.PathNamespace)
	add("interface", m.Interface)
	add("member", m.Member)
	keys := make([]int, 0, len(m.Args))
	for k := range m.Args {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	for _, k := range keys {
		add("arg"+strconv.Itoa(k), m.Args[k])
	}
	return strings.Join(parts, ",")
}

// matches reports whether sig satisfies the rule. owners maps well-known names
// to their current unique owner, since signals carry the unique name.
func (m Match) matches(sig *dbus.Signal, owners map[string]string) bool {
	if m.Sender != "" && sig.Sender != m.Sender {
		if strings.HasPrefix(m.Sender, ":") || owners[m.Sender] == "" || owners[m.Sender] != sig.Sender {
			// The bus itself sends with its well-known name.
			if !(m.Sender == "org.freedesktop.DBus" && sig.Sender == "org.freedesktop.DBus") {
				return false
			}
		}
	}
	if m.Path != "" && string(sig.Path) != m.Path {
		return false
	}
	if m.PathNamespace != "" {
		p := string(sig.Path)
		if p != m.PathNamespace && !strings.HasPrefix(p, strings.TrimSuffix(m.PathNamespace, "/")+"/") {
			return false
		}
	}
	dot := strings.LastIndexByte(sig.Name, '.')
	if dot < 0 {
		return false
	}
	iface, member := sig.Name[:dot], sig.Name[dot+1:]
	if m.Interface != "" && iface != m.Interface {
		return false
	}
	if m.Member != "" && member != m.Member {
		return false
	}
	for i, want := range m.Args {
		if i >= len(sig.Body) {
			return false
		}
		got, ok := sig.Body[i].(string)
		if !ok {
			if p, isPath := sig.Body[i].(dbus.ObjectPath); isPath {
				got, ok = string(p), true
			}
		}
		if !ok || got != want {
			return false
		}
	}
	return true
}
