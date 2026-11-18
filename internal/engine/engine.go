// Package engine is the reconcile loop: it keeps fact values current,
// evaluates rules, runs enter/exit actions and makes outputs match what the
// matching rules want.
//
// All engine state is owned by the goroutine running Run. Everything else
// (sources, IPC handlers, signal handlers) talks to it by posting closures.
package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/PeterSR/systemd-whilst/internal/action"
	"github.com/PeterSR/systemd-whilst/internal/bus"
	"github.com/PeterSR/systemd-whilst/internal/config"
	"github.com/PeterSR/systemd-whilst/internal/expr"
	"github.com/PeterSR/systemd-whilst/internal/output"
	"github.com/PeterSR/systemd-whilst/internal/source"
	"github.com/PeterSR/systemd-whilst/internal/value"
)

// Deps are the engine's connections to the outside world. Tests replace them
// with fakes.
type Deps struct {
	Hub       *bus.Hub // nil when nothing needs the bus directly
	NewSource func(*config.Fact) (source.Source, error)
	Outputs   []output.Output
	Actions   action.Runner
	// StatePath is where state is persisted; empty disables persistence.
	StatePath string
	// BootID identifies the current boot; see the state file.
	BootID string
}

// maxPasses bounds how often one evaluation may loop because set actions
// changed state facts, so a rule set that flips forever cannot hang whilst.
const maxPasses = 8

// retryAfter is how soon a failed output is retried.
const retryAfter = 30 * time.Second

// Engine is the running state.
type Engine struct {
	deps    Deps
	comp    *Compiled
	facts   map[string]*Fact
	rules   []*Rule // config rules, highest priority first
	temps   []*Rule
	outputs []output.Output

	ch       chan func()
	dirty    bool
	settled  bool
	settleBy time.Time
	waiting  map[string]bool // facts whose source has not reported yet

	srcCtx    context.Context
	srcCancel context.CancelFunc
	runCtx    context.Context

	started     time.Time
	gen         int // incremented whenever sources are restarted
	nextWake    time.Time
	outputErr   bool
	tempSeq     int
	lastSaved   []byte
	configError string
}

// Fact is a fact's runtime state.
type Fact struct {
	Def *FactDef
	// Value is valid when Has is set.
	Value any
	Has   bool
	// Provisional is set while the value is an initial or carried-over guess
	// rather than an observation in this boot. Replacing a provisional value
	// is not counted as a change.
	Provisional bool
	// Changed is when the value last changed; zero if never seen to change.
	Changed time.Time
	Updated time.Time
	Err     string
	pending *pending
	src     source.Source
	lastSeq int64 // highest Update.Seq applied
}

type pending struct {
	value any
	due   time.Time
}

// Rule is a rule's runtime state.
type Rule struct {
	Def     *RuleDef
	Matched bool
	Since   time.Time // when Matched last changed
	// FalseSince is set while the rule lingers: it still matches, but its
	// condition has been false since then.
	FalseSince time.Time
	Err        string
	// Waiting is set when Err only means a fact has no confirmed value
	// yet, as opposed to a condition that cannot work.
	Waiting bool
	Temp    *Temp
}

// New prepares an engine for a compiled config. Call Run to start it.
func New(comp *Compiled, deps Deps) *Engine {
	e := &Engine{
		deps:    deps,
		outputs: deps.Outputs,
		ch:      make(chan func(), 1024),
		started: now(),
	}
	e.install(comp, nil, nil)
	return e
}

// rereadable reports whether a fact of this kind gets a real reading soon on
// its own. Only those facts can hold provisional guesses: a signal fact or a
// state fact may never be "read", so its initial value is taken as real.
func rereadable(kind string) bool {
	switch kind {
	case config.SrcDBus, config.SrcUnit, config.SrcCommand, config.SrcFile, config.SrcExpr:
		return true
	}
	return false
}

// initial gives a fact its configured initial value, if any.
func (e *Engine) initial(f *Fact) {
	if f.Def.HasInitial {
		f.Value, f.Has = f.Def.Initial, true
		f.Provisional = rereadable(f.Def.Kind)
	}
}

// now returns wall-clock time without a monotonic reading, so durations
// include time spent suspended.
func now() time.Time { return time.Now().Round(0) }

// install switches to a compiled config, carrying over fact values and rule
// match state by name.
func (e *Engine) install(comp *Compiled, oldFacts map[string]*Fact, oldRules []*Rule) {
	e.comp = comp
	e.facts = map[string]*Fact{}
	for name, d := range comp.Facts {
		f := &Fact{Def: d}
		old, ok := oldFacts[name]
		carry := ok && old.Has && old.Def.Kind == d.Kind
		switch {
		case carry && d.Kind == config.SrcState:
			// A state fact keeps its value as long as it is still allowed,
			// even if the list of allowed values changed.
			if v, err := value.Coerce(old.Value, d.Type); err == nil && d.CheckState(v) == nil {
				f.Value, f.Has, f.Changed, f.Updated = v, true, old.Changed, old.Updated
			} else {
				e.initial(f)
			}
		case carry && sourceKey(old.Def.Fact) == sourceKey(d.Fact) && old.Def.Type == d.Type && old.Def.Transform == d.Transform:
			f.Value, f.Has, f.Provisional, f.Changed, f.Updated = old.Value, true, old.Provisional, old.Changed, old.Updated
		case carry && rereadable(d.Kind):
			// The definition changed: keep the old value as a guess until
			// the new source reads, if it still fits.
			if v, err := value.Coerce(old.Value, d.Type); err == nil {
				// since() keeps running: replacing a guess is not a change.
				f.Value, f.Has, f.Provisional, f.Changed = v, true, true, old.Changed
			} else {
				e.initial(f)
			}
		default:
			e.initial(f)
		}
		e.facts[name] = f
	}
	prev := map[string]*Rule{}
	for _, r := range oldRules {
		prev[r.Def.Name] = r
	}
	e.rules = nil
	for _, d := range comp.Rules {
		r := &Rule{Def: d}
		if old, ok := prev[d.Name]; ok {
			r.Matched, r.Since, r.FalseSince = old.Matched, old.Since, old.FalseSince
		}
		e.rules = append(e.rules, r)
	}
}

// post queues fn to run on the engine goroutine.
func (e *Engine) post(fn func()) {
	e.ch <- fn
}

// Do runs fn on the engine goroutine and waits for it.
func (e *Engine) Do(fn func()) {
	done := make(chan struct{})
	e.post(func() { fn(); close(done) })
	<-done
}

// Run starts the sources and runs the loop until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	e.runCtx = ctx
	e.settleBy = now().Add(e.comp.Config.Settle)
	e.startSources()
	// Re-apply restored rule state (same boot) at once rather than after
	// settle, so a restart of whilst does not drop locks for seconds.
	e.reconcile()
	timer := time.NewTimer(time.Until(e.settleBy))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			e.shutdown()
			return nil
		case fn := <-e.ch:
			fn()
		case <-timer.C:
			e.dirty = true
		}
	drain:
		for {
			select {
			case fn := <-e.ch:
				fn()
			default:
				break drain
			}
		}
		t := now()
		if !e.settled && (len(e.waiting) == 0 || !t.Before(e.settleBy)) {
			if len(e.waiting) > 0 {
				slog.Warn("starting before every source reported", "waiting", sortedSet(e.waiting))
			}
			e.settled = true
			e.dirty = true
		}
		if e.settled && (e.dirty || (!e.nextWake.IsZero() && !t.Before(e.nextWake))) {
			e.dirty = false
			e.Evaluate(t)
		}
		wake := e.nextWake
		if !e.settled {
			wake = e.settleBy
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if !wake.IsZero() {
			timer.Reset(max(time.Until(wake), time.Millisecond))
		}
	}
}

func (e *Engine) startSources() {
	ctx := e.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	e.srcCtx, e.srcCancel = context.WithCancel(ctx)
	e.waiting = map[string]bool{}
	e.gen++
	gen := e.gen
	for _, name := range e.comp.Config.FactOrder {
		f := e.facts[name]
		if e.deps.NewSource == nil {
			continue
		}
		src, err := e.deps.NewSource(f.Def.Fact)
		if err != nil {
			f.Err = err.Error()
			slog.Error("cannot create source", "fact", name, "error", err)
			continue
		}
		if src == nil {
			continue
		}
		f.src = src
		if f.Def.Kind != config.SrcDBusSignal {
			e.waiting[name] = true
		}
		// Updates from sources of an earlier config are dropped, even when
		// the new config has a fact with the same name.
		emit := func(u source.Update) {
			e.post(func() {
				if gen == e.gen {
					e.onUpdate(u)
				}
			})
		}
		// Starting may make D-Bus calls; keep them off the engine goroutine.
		// A source that cannot start (a service not up yet at login) is
		// retried with backoff until the config is replaced.
		go func(name string, src source.Source, ctx context.Context) {
			delay := 5 * time.Second
			for {
				// Each attempt gets its own context, so the subscriptions
				// a failed attempt did make are undone before retrying.
				attempt, cancel := context.WithCancel(ctx)
				err := src.Start(attempt, emit)
				if err == nil {
					// Lives until the config is replaced.
					go func() { <-ctx.Done(); cancel() }()
					return
				}
				cancel()
				if ctx.Err() != nil {
					return
				}
				emit(source.Update{Fact: name, Err: fmt.Errorf("cannot start source: %w", err)})
				if strings.Contains(err.Error(), "MatchRuleInvalid") {
					slog.Error("cannot start source; the bus rejected its match rule, not retrying", "fact", name, "error", err)
					return
				}
				slog.Error("cannot start source; will retry", "fact", name, "error", err, "in", delay)
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
				delay = min(delay*2, 5*time.Minute)
			}
		}(name, src, e.srcCtx)
	}
}

func (e *Engine) stopSources() {
	if e.srcCancel != nil {
		e.srcCancel()
	}
}

func (e *Engine) shutdown() {
	e.stopSources()
	e.save(true)
	for _, o := range e.outputs {
		o.Close()
	}
}

// RefreshAll asks every source to re-read, for example after resume.
func (e *Engine) RefreshAll() {
	for _, f := range e.facts {
		if f.src != nil {
			f.src.Refresh()
		}
	}
	e.dirty = true
}

// Wake marks the engine for evaluation on the next loop iteration.
func (e *Engine) Wake() { e.post(func() { e.dirty = true }) }

func (e *Engine) onUpdate(u source.Update) {
	f, ok := e.facts[u.Fact]
	if !ok {
		return
	}
	if u.Seq != 0 {
		if u.Seq < f.lastSeq {
			return // an older read that lost the race with a pushed value
		}
		f.lastSeq = u.Seq
	}
	delete(e.waiting, u.Fact)
	t := now()
	if u.Err != nil {
		if f.Err != u.Err.Error() {
			slog.Warn("cannot read fact", "fact", u.Fact, "error", u.Err)
		}
		f.Err = u.Err.Error()
		e.dirty = true
		return
	}
	v, err := e.convert(f, u.Value)
	if err != nil {
		f.Err = err.Error()
		e.dirty = true
		return
	}
	f.Err = ""
	e.offer(f, v, t)
}

// convert applies a fact's transform and declared type to a raw value.
func (e *Engine) convert(f *Fact, raw any) (any, error) {
	v := raw
	if p := f.Def.TransProg; p != nil {
		out, err := p.Eval(map[string]any{"value": raw}, nil)
		if err != nil {
			return nil, fmt.Errorf("transform: %w", err)
		}
		v = out
	}
	return value.Coerce(v, f.Def.Type)
}

// offer proposes a new value, honouring debounce.
func (e *Engine) offer(f *Fact, v any, t time.Time) {
	f.Updated = t
	if f.Has && value.Equal(f.Value, v) {
		// Keep the fresh value: equal numbers may differ in kind (80 and
		// 80.0) after a round trip through the state file.
		f.Value = v
		if f.pending != nil {
			f.pending = nil
			e.dirty = true
		}
		if f.Provisional {
			f.Provisional = false
			e.dirty = true
		}
		return
	}
	if d := f.Def.Debounce; d > 0 && f.Has {
		switch {
		case f.pending == nil:
			f.pending = &pending{value: v, due: t.Add(d)}
			e.dirty = true
		case f.Provisional:
			// Replacing a guess: the first reading that differs starts
			// the clock, and whatever is current when it runs out is
			// taken. A fact that keeps changing still gets confirmed.
			f.pending.value = v
		case !value.Equal(f.pending.value, v):
			f.pending = &pending{value: v, due: t.Add(d)}
			e.dirty = true
		}
		return
	}
	e.set(f, v, t)
}

// set stores a value and records the change.
func (e *Engine) set(f *Fact, v any, t time.Time) bool {
	f.pending = nil
	if f.Has && value.Equal(f.Value, v) {
		f.Provisional = false
		return false
	}
	if f.Has && !f.Provisional {
		f.Changed = t
		slog.Info("fact changed", "fact", f.Def.Name, "from", value.Format(f.Value), "to", value.Format(v))
	} else {
		slog.Info("fact read", "fact", f.Def.Name, "value", value.Format(v))
	}
	f.Value, f.Has, f.Provisional = v, true, false
	e.dirty = true
	return true
}

// vars returns the current fact values for CEL.
func (e *Engine) vars() map[string]any {
	m := make(map[string]any, len(e.facts))
	for name, f := range e.facts {
		if f.Has {
			m[name] = f.Value
		}
	}
	return m
}

func (e *Engine) ctx(t time.Time) *expr.Context {
	return &expr.Context{Now: t, Since: func(name string) time.Duration {
		f, ok := e.facts[name]
		if !ok || f.Changed.IsZero() {
			return expr.Never
		}
		return max(t.Sub(f.Changed), 0)
	}}
}

// Evaluate runs one full evaluation at time t: debounced values, expr facts,
// rules and their actions, outputs, persistence and the next wake-up.
func (e *Engine) Evaluate(t time.Time) {
	for _, f := range e.facts {
		if f.pending != nil && !t.Before(f.pending.due) {
			e.set(f, f.pending.value, t)
		}
	}
	for pass := 0; ; pass++ {
		e.computeExprFacts(t)
		var exits, enters []*Rule
		vars := e.vars()
		ctx := e.ctx(t)
		for _, r := range e.allRules() {
			m, err := r.Def.While.EvalBool(vars, ctx)
			r.Waiting = false
			if p := e.provisionalInput(r.Def.While.Facts); p != "" && err == nil {
				// Do not act on guesses (values remembered from before a
				// reboot, initial values): keep the rule as it is until
				// the fact has really been read.
				err = fmt.Errorf("waiting for a first reading of %s", p)
				r.Waiting = true
			} else if p := cmp.Or(e.missingInput(r.Def.While.Facts), e.provisionalInput(r.Def.While.Facts)); p != "" && err != nil {
				// An input without a confirmed value can explain the error;
				// it is re-checked once the input has been read.
				err = fmt.Errorf("waiting for a first reading of %s", p)
				r.Waiting = true
			}
			if err != nil {
				if r.Err != err.Error() {
					slog.Warn("cannot evaluate rule", "rule", r.Def.Name, "error", err)
				}
				r.Err = err.Error()
				m = r.Matched
			} else {
				r.Err = ""
			}
			if r.Temp != nil && r.Temp.finished(t) {
				m = false
			} else if err == nil {
				m = r.linger(m, t)
			}
			if m == r.Matched {
				continue
			}
			r.Matched, r.Since, r.FalseSince = m, t, time.Time{}
			if m {
				slog.Info("rule matches", "rule", r.Def.Name)
				enters = append(enters, r)
			} else {
				slog.Info("rule no longer matches", "rule", r.Def.Name)
				exits = append(exits, r)
			}
		}
		changed := false
		for _, r := range exits {
			changed = e.runActions(r, "exit", r.Def.Exit, t) || changed
		}
		for _, r := range enters {
			changed = e.runActions(r, "enter", r.Def.Enter, t) || changed
		}
		if !changed {
			break
		}
		if pass+1 >= maxPasses {
			slog.Error("set actions keep changing state; stopping this evaluation", "passes", maxPasses)
			break
		}
	}
	e.reconcile()
	e.dropFinishedTemps(t)
	e.save(false)
	e.schedule(t)
}

func (e *Engine) computeExprFacts(t time.Time) {
	vars := e.vars()
	ctx := e.ctx(t)
	for _, name := range e.comp.ExprOrder {
		f := e.facts[name]
		out, err := f.Def.Prog.Eval(vars, ctx)
		if err == nil {
			out, err = value.Coerce(out, f.Def.Type)
		}
		if err != nil {
			if f.Err != err.Error() {
				slog.Warn("cannot evaluate fact", "fact", name, "error", err)
			}
			f.Err = err.Error()
			continue
		}
		f.Err = ""
		// A value computed from a guess is itself a guess: replacing it
		// later must not count as a change.
		guess := e.anyProvisional(f.Def.Prog.Facts)
		if guess && f.Has && !value.Equal(f.Value, out) {
			f.Provisional = true
		}
		e.offer(f, out, t)
		f.Provisional = f.Provisional || guess
		vars[name] = f.Value
	}
}

// provisionalInput returns the first of the named facts whose value is
// still a guess, or "".
func (e *Engine) provisionalInput(names []string) string {
	for _, n := range names {
		if f, ok := e.facts[n]; ok && f.Has && f.Provisional {
			return n
		}
	}
	return ""
}

// missingInput returns the first of the named facts that has no value at
// all, or "".
func (e *Engine) missingInput(names []string) string {
	for _, n := range names {
		if f, ok := e.facts[n]; ok && !f.Has {
			return n
		}
	}
	return ""
}

func (e *Engine) anyProvisional(names []string) bool { return e.provisionalInput(names) != "" }

func (e *Engine) runActions(r *Rule, event string, actions []config.Action, t time.Time) bool {
	changed := false
	var others []config.Action
	for _, a := range actions {
		if a.Set != nil {
			names := make([]string, 0, len(a.Set))
			for n := range a.Set {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				f, ok := e.facts[n]
				if !ok {
					continue
				}
				v, err := value.Coerce(a.Set[n], f.Def.Type)
				if err == nil {
					err = f.Def.CheckState(v)
				}
				if err != nil {
					// Possible for exit actions of a rule that a reload
					// removed, checked against the old config.
					slog.Warn("ignoring set action", "rule", r.Def.Name, "fact", n, "error", err)
					continue
				}
				if e.set(f, v, t) {
					changed = true
				}
			}
			continue
		}
		others = append(others, a)
	}
	if len(others) > 0 && e.deps.Actions != nil {
		e.deps.Actions.Run(r.Def.Name, event, others)
	}
	return changed
}

// linger applies the rule's linger time to a freshly evaluated condition and
// returns whether the rule should match.
func (r *Rule) linger(cond bool, t time.Time) bool {
	if cond || !r.Matched || r.Def.Linger <= 0 {
		if cond && !r.FalseSince.IsZero() {
			slog.Info("rule condition true again while lingering", "rule", r.Def.Name)
		}
		r.FalseSince = time.Time{}
		return cond
	}
	if r.FalseSince.IsZero() {
		r.FalseSince = t
		slog.Info("rule condition false; lingering", "rule", r.Def.Name, "for", r.Def.Linger)
	}
	return t.Before(r.LingerUntil())
}

// LingerUntil is when a lingering rule stops matching, or zero.
func (r *Rule) LingerUntil() time.Time {
	if r.FalseSince.IsZero() || r.Def.Linger <= 0 {
		return time.Time{}
	}
	return r.FalseSince.Add(r.Def.Linger)
}

func (e *Engine) allRules() []*Rule {
	if len(e.temps) == 0 {
		return e.rules
	}
	out := make([]*Rule, 0, len(e.temps)+len(e.rules))
	out = append(out, e.temps...)
	return append(out, e.rules...)
}

func (e *Engine) reconcile() {
	e.outputErr = false
	for _, o := range e.outputs {
		var wants []output.Want
		for _, r := range e.allRules() {
			if !r.Matched {
				continue
			}
			if claim, ok := r.Def.Outputs[o.Name()]; ok {
				wants = append(wants, output.Want{Rule: r.Def.Name, Claim: claim})
			}
		}
		if err := o.Reconcile(wants); err != nil {
			slog.Warn("cannot apply output", "output", o.Name(), "error", err)
			e.outputErr = true
		}
	}
}

// schedule works out when the next evaluation is needed.
func (e *Engine) schedule(t time.Time) {
	var next time.Time
	consider := func(w time.Time) {
		if w.After(t) && (next.IsZero() || w.Before(next)) {
			next = w
		}
	}
	ticks := false
	progs := make([]*expr.Program, 0, len(e.comp.ExprOrder)+len(e.rules)+len(e.temps))
	for _, name := range e.comp.ExprOrder {
		progs = append(progs, e.facts[name].Def.Prog)
	}
	for _, r := range e.allRules() {
		progs = append(progs, r.Def.While)
		if r.Temp != nil && !r.Temp.Until.IsZero() {
			consider(r.Temp.Until)
		}
		if u := r.LingerUntil(); !u.IsZero() {
			consider(u)
		}
	}
	for _, p := range progs {
		ticks = ticks || p.Ticks
		for _, th := range p.Thresholds {
			f := e.facts[th.Fact]
			if f == nil || f.Changed.IsZero() {
				continue
			}
			// A strict comparison flips just after the threshold.
			consider(f.Changed.Add(th.After + time.Millisecond))
		}
	}
	for _, f := range e.facts {
		if f.pending != nil {
			consider(f.pending.due)
		}
	}
	if ticks {
		consider(t.Add(e.comp.Config.Tick))
	}
	if e.outputErr {
		consider(t.Add(retryAfter))
	}
	e.nextWake = next
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Resumed is called from any goroutine after the machine wakes from sleep.
// Every source re-reads, since values may have changed while asleep.
func (e *Engine) Resumed() { e.post(e.RefreshAll) }

// defKey identifies everything about a fact's definition that shapes its
// value.
func defKey(d *FactDef) string {
	return sourceKey(d.Fact) + "|" + d.Type + "|" + d.Transform
}

// sourceKey summarises where a fact's value comes from, to tell whether a
// reload changed it.
func sourceKey(f *config.Fact) string {
	var cmd *config.Command
	if f.Command != nil {
		c := *f.Command
		c.Timeout = 0 // how long a read may take does not change the value
		cmd = &c
	}
	b, _ := json.Marshal([]any{f.Kind, f.DBus, f.DBusSignal, f.Unit, cmd, f.File, f.Expr, f.States})
	return string(b)
}
