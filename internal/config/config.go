// Package config loads and validates whilst's TOML configuration.
//
// Config files are decoded into generic maps and checked by hand, so that rule
// tables can carry output keys known only to the engine, and so that every
// unknown key is reported instead of silently ignored.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/PeterSR/systemd-whilst/internal/bus"
	"github.com/PeterSR/systemd-whilst/internal/expr"
	"github.com/PeterSR/systemd-whilst/internal/value"
	"github.com/PeterSR/systemd-whilst/presets"
)

// Config is a loaded configuration, with includes merged in.
type Config struct {
	Path  string
	Facts map[string]*Fact
	// FactOrder lists fact names in the order they were declared.
	FactOrder []string
	Rules     []*Rule
	// Tick is how often expressions that depend on the clock are re-evaluated.
	Tick time.Duration
	// Settle is how long to wait at start for every source to report before
	// the first evaluation.
	Settle time.Duration
}

// Source kinds.
const (
	SrcDBus       = "dbus"
	SrcDBusSignal = "dbus_signal"
	SrcUnit       = "unit"
	SrcCommand    = "command"
	SrcFile       = "file"
	SrcExpr       = "expr"
	SrcState      = "state"
)

var sourceKinds = []string{SrcDBus, SrcDBusSignal, SrcUnit, SrcCommand, SrcFile, SrcExpr, SrcState}

// Fact is one declared fact.
type Fact struct {
	Name   string
	Origin string // file that declared it
	Kind   string // one of the Src constants
	Type   string // value.Kinds, "" meaning dyn

	Initial    any
	HasInitial bool
	Transform  string
	Debounce   time.Duration

	// Re-read triggers, for sources that can be re-read.
	OnDBus        []bus.Match // system bus
	OnDBusSession []bus.Match
	Every         time.Duration

	DBus       *DBusProp
	DBusSignal *DBusSignal
	Unit       *Unit
	Command    *Command
	File       string
	Expr       string
	// States lists the allowed values of a state fact; nil allows any value.
	States []string
}

// DBusProp reads a D-Bus property.
type DBusProp struct {
	Bus, Service, Path, Interface, Property string
}

// DBusSignal takes its value from a signal.
type DBusSignal struct {
	Bus   string
	Match bus.Match
	// Arg selects one argument of the signal body; -1 means the whole body
	// (or its only element when it has exactly one).
	Arg int
}

// Unit reads a systemd unit's ActiveState.
type Unit struct {
	Bus  string
	Name string
}

// Command runs a shell command.
type Command struct {
	Run     string
	Output  string // "stdout", "status" or "json"
	Timeout time.Duration
}

// Rule is one [[rule]] table.
type Rule struct {
	Name     string
	Origin   string
	While    string
	Priority int
	// HasPriority is false when the rule relies on file order.
	HasPriority bool
	Enter       []Action
	Exit        []Action
	// Outputs holds the remaining keys, validated by the engine.
	Outputs map[string]any
}

// Reserved rule keys; every other key in a rule is an output.
var ruleKeys = map[string]bool{"name": true, "while": true, "priority": true, "enter": true, "exit": true}

// DefaultPath returns the config path whilst uses when none is given.
func DefaultPath() string {
	if p := os.Getenv("WHILST_CONFIG"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(dir) {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "whilst", "config.toml")
}

// Empty returns a configuration with no facts and no rules.
func Empty(path string) *Config {
	return &Config{Path: path, Facts: map[string]*Fact{}, Tick: 30 * time.Second, Settle: 3 * time.Second}
}

// Load reads the config at path and everything it includes.
func Load(path string) (*Config, error) {
	c := Empty(path)
	l := &loader{cfg: c, seen: map[string]bool{}}
	if err := l.file(path, true, 0); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Parse loads a config from a string, for tests.
func Parse(src string) (*Config, error) {
	c := Empty("(inline)")
	l := &loader{cfg: c, seen: map[string]bool{}}
	var raw map[string]any
	if _, err := toml.Decode(src, &raw); err != nil {
		return nil, err
	}
	if err := l.doc(raw, "(inline)", ".", true, 0); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

type loader struct {
	cfg  *Config
	seen map[string]bool
}

func (l *loader) file(path string, top bool, depth int) error {
	var data []byte
	var err error
	dir := "."
	origin := path
	if name, ok := strings.CutPrefix(path, "preset:"); ok {
		if data, err = presets.Files.ReadFile(name + ".toml"); err != nil {
			return fmt.Errorf("include %q: no such preset (have: %s)", path, strings.Join(presets.Names(), ", "))
		}
	} else {
		if abs, aerr := filepath.Abs(path); aerr == nil {
			origin = abs
		}
		if data, err = os.ReadFile(origin); err != nil {
			return err
		}
		dir = filepath.Dir(origin)
	}
	if l.seen[origin] {
		return fmt.Errorf("%s is included more than once", origin)
	}
	l.seen[origin] = true
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return fmt.Errorf("%s: %w", origin, err)
	}
	return l.doc(raw, origin, dir, top, depth)
}

func (l *loader) doc(raw map[string]any, origin, dir string, top bool, depth int) error {
	for _, k := range sortedKeys(raw) {
		v := raw[k]
		switch k {
		case "include":
			list, err := stringList(v)
			if err != nil {
				return fmt.Errorf("%s: include: %w", origin, err)
			}
			if depth >= 8 {
				return fmt.Errorf("%s: includes nested too deeply", origin)
			}
			for _, inc := range list {
				p := inc
				if !strings.HasPrefix(p, "preset:") {
					p = expandHome(p)
					if !filepath.IsAbs(p) {
						p = filepath.Join(dir, p)
					}
				}
				if err := l.file(p, false, depth+1); err != nil {
					return fmt.Errorf("%s: %w", origin, err)
				}
			}
		case "fact":
			facts, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: fact must be a table of tables, like [fact.ac]", origin)
			}
			for _, name := range sortedKeys(facts) {
				ft, ok := facts[name].(map[string]any)
				if !ok {
					return fmt.Errorf("%s: fact.%s must be a table", origin, name)
				}
				if prev, dup := l.cfg.Facts[name]; dup {
					return fmt.Errorf("%s: fact %s is already declared in %s", origin, name, prev.Origin)
				}
				f, err := parseFact(name, ft)
				if err != nil {
					return fmt.Errorf("%s: fact %s: %w", origin, name, err)
				}
				f.Origin = origin
				l.cfg.Facts[name] = f
				l.cfg.FactOrder = append(l.cfg.FactOrder, name)
			}
		case "rule":
			rules, ok := v.([]map[string]any)
			if !ok {
				return fmt.Errorf("%s: rule must be an array of tables, like [[rule]]", origin)
			}
			for i, rt := range rules {
				r, err := parseRule(rt)
				if err != nil {
					label := fmt.Sprintf("rule %d", i+1)
					if n, ok := rt["name"].(string); ok {
						label = "rule " + n
					}
					return fmt.Errorf("%s: %s: %w", origin, label, err)
				}
				r.Origin = origin
				l.cfg.Rules = append(l.cfg.Rules, r)
			}
		case "tick", "settle":
			if !top {
				return fmt.Errorf("%s: %s can only be set in the main config", origin, k)
			}
			d, err := durationValue(v)
			if err != nil {
				return fmt.Errorf("%s: %s: %w", origin, k, err)
			}
			if d <= 0 {
				return fmt.Errorf("%s: %s must be positive", origin, k)
			}
			if k == "tick" {
				l.cfg.Tick = d
			} else {
				l.cfg.Settle = d
			}
		default:
			return fmt.Errorf("%s: unknown key %q (expected include, fact, rule, tick or settle)", origin, k)
		}
	}
	return nil
}

func parseFact(name string, t map[string]any) (*Fact, error) {
	if err := expr.ValidName(name); err != nil {
		return nil, err
	}
	f := &Fact{Name: name}
	var kinds []string
	for _, k := range sourceKinds {
		if _, ok := t[k]; ok {
			kinds = append(kinds, k)
		}
	}
	switch len(kinds) {
	case 0:
		return nil, fmt.Errorf("needs a source: one of %s", strings.Join(sourceKinds, ", "))
	case 1:
		f.Kind = kinds[0]
	default:
		return nil, fmt.Errorf("has more than one source (%s)", strings.Join(kinds, ", "))
	}
	allowed := map[string]bool{f.Kind: true, "type": true, "initial": true, "debounce": true}
	rereadable := false
	switch f.Kind {
	case SrcDBus, SrcUnit, SrcCommand, SrcFile:
		rereadable = true
		allowed["transform"] = true
	case SrcDBusSignal:
		allowed["transform"] = true
	}
	if rereadable {
		allowed["on_dbus"] = true
		allowed["on_dbus_session"] = true
		allowed["every"] = true
	}
	if f.Kind == SrcCommand {
		allowed["output"] = true
		allowed["timeout"] = true
	}
	for _, k := range sortedKeys(t) {
		if !allowed[k] {
			return nil, fmt.Errorf("unknown key %q for a %s fact", k, f.Kind)
		}
	}

	var err error
	if v, ok := t["type"]; ok {
		s, ok := v.(string)
		if !ok || !contains(value.Kinds, s) {
			return nil, fmt.Errorf("type must be one of %s", strings.Join(value.Kinds, ", "))
		}
		f.Type = s
	}
	if v, ok := t["initial"]; ok {
		f.Initial = value.Normalize(v)
		f.HasInitial = true
	}
	if v, ok := t["transform"]; ok {
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("transform must be a CEL expression string")
		}
		f.Transform = s
	}
	if v, ok := t["debounce"]; ok {
		if f.Debounce, err = durationValue(v); err != nil {
			return nil, fmt.Errorf("debounce: %w", err)
		}
	}
	if v, ok := t["every"]; ok {
		if f.Every, err = durationValue(v); err != nil {
			return nil, fmt.Errorf("every: %w", err)
		}
		if f.Every < time.Second {
			return nil, fmt.Errorf("every must be at least 1s")
		}
	}
	for key, dst := range map[string]*[]bus.Match{"on_dbus": &f.OnDBus, "on_dbus_session": &f.OnDBusSession} {
		v, ok := t[key]
		if !ok {
			continue
		}
		rules, err := stringList(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		for _, r := range rules {
			m, err := bus.ParseMatch(r)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			*dst = append(*dst, m)
		}
	}

	src := t[f.Kind]
	switch f.Kind {
	case SrcDBus:
		tbl, ok := src.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("dbus must be a table with service, path, interface and property")
		}
		p := &DBusProp{}
		if err := fields(tbl, map[string]*string{"bus": &p.Bus, "service": &p.Service, "path": &p.Path, "interface": &p.Interface, "property": &p.Property}, "service", "path", "interface", "property"); err != nil {
			return nil, fmt.Errorf("dbus: %w", err)
		}
		if p.Bus, err = bus.Normalize(p.Bus, bus.System); err != nil {
			return nil, fmt.Errorf("dbus: %w", err)
		}
		for _, chk := range []error{bus.ValidBusName(p.Service), bus.ValidPath(p.Path), bus.ValidInterface(p.Interface), bus.ValidMember(p.Property)} {
			if chk != nil {
				return nil, fmt.Errorf("dbus: %w", chk)
			}
		}
		f.DBus = p
	case SrcDBusSignal:
		tbl, ok := src.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("dbus_signal must be a table")
		}
		s := &DBusSignal{Arg: -1}
		var rule string
		var arg any
		if a, ok := tbl["arg"]; ok {
			arg = a
			delete(tbl, "arg")
		}
		m := bus.Match{Type: "signal"}
		if err := fields(tbl, map[string]*string{"bus": &s.Bus, "match": &rule, "sender": &m.Sender, "path": &m.Path, "interface": &m.Interface, "member": &m.Member}); err != nil {
			return nil, fmt.Errorf("dbus_signal: %w", err)
		}
		if rule != "" {
			if m.Sender != "" || m.Path != "" || m.Interface != "" || m.Member != "" {
				return nil, fmt.Errorf("dbus_signal: give either match or sender/path/interface/member, not both")
			}
			if m, err = bus.ParseMatch(rule); err != nil {
				return nil, fmt.Errorf("dbus_signal: %w", err)
			}
		}
		if m.Member == "" {
			return nil, fmt.Errorf("dbus_signal: member is required")
		}
		if err := m.Validate(); err != nil {
			return nil, fmt.Errorf("dbus_signal: %w", err)
		}
		s.Match = m
		if arg != nil {
			i, ok := arg.(int64)
			if !ok || i < 0 || i > 63 {
				return nil, fmt.Errorf("dbus_signal: arg must be a non-negative integer")
			}
			s.Arg = int(i)
		}
		if s.Bus, err = bus.Normalize(s.Bus, bus.System); err != nil {
			return nil, fmt.Errorf("dbus_signal: %w", err)
		}
		f.DBusSignal = s
	case SrcUnit:
		u := &Unit{}
		switch x := src.(type) {
		case string:
			u.Name = x
		case map[string]any:
			if err := fields(x, map[string]*string{"name": &u.Name, "bus": &u.Bus}, "name"); err != nil {
				return nil, fmt.Errorf("unit: %w", err)
			}
		default:
			return nil, fmt.Errorf("unit must be a unit name or a table with name and bus")
		}
		if err := ValidUnitName(u.Name); err != nil {
			return nil, fmt.Errorf("unit: %w", err)
		}
		if u.Bus, err = bus.Normalize(u.Bus, bus.Session); err != nil {
			return nil, fmt.Errorf("unit: %w", err)
		}
		f.Unit = u
	case SrcCommand:
		run, ok := src.(string)
		if !ok || strings.TrimSpace(run) == "" {
			return nil, fmt.Errorf("command must be a non-empty string")
		}
		c := &Command{Run: run, Output: "stdout", Timeout: 10 * time.Second}
		if v, ok := t["output"]; ok {
			s, _ := v.(string)
			if s != "stdout" && s != "status" && s != "json" {
				return nil, fmt.Errorf("output must be stdout, status or json")
			}
			c.Output = s
		}
		if v, ok := t["timeout"]; ok {
			if c.Timeout, err = durationValue(v); err != nil || c.Timeout <= 0 {
				return nil, fmt.Errorf("timeout must be a positive duration")
			}
		}
		f.Command = c
	case SrcFile:
		p, ok := src.(string)
		if !ok || p == "" {
			return nil, fmt.Errorf("file must be a path")
		}
		f.File = expandHome(p)
		if f.Every == 0 {
			f.Every = 5 * time.Second
		}
	case SrcExpr:
		s, ok := src.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("expr must be a CEL expression string")
		}
		f.Expr = s
	case SrcState:
		switch x := src.(type) {
		case bool:
			if !x {
				return nil, fmt.Errorf("state = false makes no sense; use true or a list of allowed values")
			}
		default:
			list, err := stringList(x)
			if err != nil || len(list) == 0 {
				return nil, fmt.Errorf("state must be true or a non-empty list of allowed values")
			}
			f.States = list
			if f.Type == "" {
				f.Type = "string"
			}
		}
		if !f.HasInitial {
			if f.States == nil {
				return nil, fmt.Errorf("a state fact needs an initial value")
			}
			f.Initial = f.States[0]
			f.HasInitial = true
		}
		if v, err := value.Coerce(f.Initial, f.Type); err == nil {
			f.Initial = v
		}
		if err := f.CheckState(f.Initial); err != nil {
			return nil, fmt.Errorf("initial: %w", err)
		}
	}
	if f.HasInitial && f.Kind != SrcState {
		if f.Initial, err = value.Coerce(f.Initial, f.Type); err != nil {
			return nil, fmt.Errorf("initial: %w", err)
		}
	}
	return f, nil
}

// CheckState validates a value for a state fact.
func (f *Fact) CheckState(v any) error {
	if f.Kind != SrcState {
		return fmt.Errorf("%s is not a state fact", f.Name)
	}
	if f.States != nil {
		s, ok := v.(string)
		if !ok || !contains(f.States, s) {
			return fmt.Errorf("%s must be one of %s, not %s", f.Name, strings.Join(f.States, ", "), value.Format(v))
		}
	}
	if f.Type != "" && f.Type != "dyn" && value.Kind(v) != f.Type {
		return fmt.Errorf("%s must be a %s, not %s", f.Name, f.Type, value.Format(v))
	}
	return nil
}

func parseRule(t map[string]any) (*Rule, error) {
	r := &Rule{Outputs: map[string]any{}}
	if v, ok := t["name"]; ok {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("name must be a non-empty string")
		}
		r.Name = s
	}
	w, ok := t["while"]
	if !ok {
		return nil, fmt.Errorf("while is required")
	}
	if r.While, ok = w.(string); !ok || strings.TrimSpace(r.While) == "" {
		return nil, fmt.Errorf("while must be a CEL condition string")
	}
	if v, ok := t["priority"]; ok {
		i, ok := v.(int64)
		if !ok {
			return nil, fmt.Errorf("priority must be an integer")
		}
		r.Priority = int(i)
		r.HasPriority = true
	}
	var err error
	if v, ok := t["enter"]; ok {
		if r.Enter, err = parseActions(v); err != nil {
			return nil, fmt.Errorf("enter: %w", err)
		}
	}
	if v, ok := t["exit"]; ok {
		if r.Exit, err = parseActions(v); err != nil {
			return nil, fmt.Errorf("exit: %w", err)
		}
	}
	for k, v := range t {
		if !ruleKeys[k] {
			r.Outputs[k] = v
		}
	}
	return r, nil
}

func (c *Config) validate() error {
	names := map[string]bool{}
	for _, r := range c.Rules {
		if strings.HasPrefix(r.Name, "ensure:") {
			return fmt.Errorf("rule %s: names starting with ensure: are reserved for `whilst ensure`", r.Name)
		}
		if r.Name != "" {
			if names[r.Name] {
				return fmt.Errorf("two rules are named %q; rule names must be unique", r.Name)
			}
			names[r.Name] = true
		}
	}
	for _, r := range c.Rules {
		if r.Name == "" {
			// Rule state is persisted and carried over by name, so an
			// unnamed rule is named after its content rather than its
			// position: reordering rules must not swap their state.
			base := "rule-" + contentHash(r)
			r.Name = base
			for i := 2; names[r.Name]; i++ {
				r.Name = fmt.Sprintf("%s-%d", base, i)
			}
			names[r.Name] = true
		}
		for _, a := range append(append([]Action{}, r.Enter...), r.Exit...) {
			for fact, v := range a.Set {
				f, ok := c.Facts[fact]
				if !ok {
					return fmt.Errorf("rule %s: set: no fact named %s", r.Name, fact)
				}
				if cv, err := value.Coerce(v, f.Type); err == nil {
					v = cv
				}
				if err := f.CheckState(v); err != nil {
					return fmt.Errorf("rule %s: set: %w", r.Name, err)
				}
			}
		}
	}
	return nil
}

func contentHash(r *Rule) string {
	b, _ := json.Marshal(struct {
		While   string
		Outputs map[string]any
		Enter   []Action
		Exit    []Action
	}{r.While, r.Outputs, r.Enter, r.Exit})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

// ValidUnitName does a light check of a systemd unit name.
func ValidUnitName(name string) error {
	if name == "" || strings.ContainsAny(name, "/ \t\n") {
		return fmt.Errorf("%q is not a valid unit name", name)
	}
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return fmt.Errorf("%q needs a unit type suffix, like .service or .timer", name)
	}
	switch name[dot+1:] {
	case "service", "socket", "target", "timer", "path", "mount", "automount", "swap", "slice", "scope", "device":
		return nil
	}
	return fmt.Errorf("%q has an unknown unit type", name)
}

// helpers

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func stringList(v any) ([]string, error) {
	switch x := v.(type) {
	case string:
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("expected a list of strings")
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return x, nil
	}
	return nil, fmt.Errorf("expected a string or a list of strings")
}

// StringList is exported for output parsers in other packages.
func StringList(v any) ([]string, error) { return stringList(v) }

// fields copies string fields out of a table, rejecting unknown keys.
func fields(t map[string]any, dst map[string]*string, required ...string) error {
	for _, k := range sortedKeys(t) {
		p, ok := dst[k]
		if !ok {
			return fmt.Errorf("unknown key %q", k)
		}
		s, ok := t[k].(string)
		if !ok {
			return fmt.Errorf("%s must be a string", k)
		}
		*p = s
	}
	for _, k := range required {
		if *dst[k] == "" {
			return fmt.Errorf("%s is required", k)
		}
	}
	return nil
}

func durationValue(v any) (time.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("expected a duration string like \"30s\" or \"5m\"")
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use a unit, like \"30s\", \"5m\" or \"1h\")", s)
	}
	return d, nil
}

// ParseDuration accepts Go durations ("1h30m") and plain numbers of hours
// ("1.5"). It is for the command line only;
// config durations always need a unit.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if h, err := strconv.ParseFloat(s, 64); err == nil && h >= 0 {
		return time.Duration(h * float64(time.Hour)), nil
	}
	return 0, fmt.Errorf("invalid duration %q", s)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}
