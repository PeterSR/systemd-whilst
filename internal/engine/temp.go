package engine

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/PeterSR/systemd-whilst/internal/bus"
)

// TempSpec describes a temporary rule added from the command line with
// `whilst ensure`.
type TempSpec struct {
	Name    string         `json:"name,omitempty"`
	While   string         `json:"while,omitempty"`
	Until   time.Time      `json:"until,omitzero"`
	Outputs map[string]any `json:"outputs"`
	// Attach ties the rule to the caller's bus connection: it is dropped when
	// the caller exits.
	Attach bool `json:"attach,omitempty"`
}

// Temp is a temporary rule's extra state.
type Temp struct {
	ID      string
	Spec    TempSpec
	Until   time.Time
	Owner   string // unique bus name of an attached caller
	Created time.Time
	gone    bool // released, or its owner exited
	unwatch func()
}

func (t *Temp) finished(at time.Time) bool {
	return t.gone || (!t.Until.IsZero() && !at.Before(t.Until))
}

// tempPriority puts temporary rules ahead of every config rule.
const tempPriority = 1 << 30

func (e *Engine) compileTemp(id string, spec TempSpec) (*Rule, error) {
	while := strings.TrimSpace(spec.While)
	if while == "" {
		while = "true"
	}
	if len(spec.Outputs) == 0 {
		return nil, fmt.Errorf("name at least one output, like inhibit=sleep")
	}
	name := spec.Name
	if name == "" {
		name = id
	}
	name = "ensure:" + name
	for _, r := range e.allRules() {
		if r.Temp != nil && r.Temp.gone {
			continue // ended; dropped at the next evaluation
		}
		if r.Def.Name == name {
			return nil, fmt.Errorf("a rule named %s already exists", name)
		}
	}
	byName, names := e.outputIndex()
	d, err := compileRule(e.comp.Env, name, while, spec.Outputs, byName, names)
	if err != nil {
		return nil, err
	}
	d.Origin = "command line"
	d.Priority = tempPriority
	return &Rule{Def: d, Temp: &Temp{ID: id, Spec: spec, Until: spec.Until}}, nil
}

// Ensure adds a temporary rule. sender is the caller's unique bus name, used
// when the spec asks to attach. It must run on the engine goroutine.
func (e *Engine) Ensure(spec TempSpec, sender string) (string, error) {
	t := now()
	if !spec.Until.IsZero() && !spec.Until.After(t) {
		return "", fmt.Errorf("that time has already passed")
	}
	e.tempSeq++
	id := fmt.Sprintf("t%d", e.tempSeq)
	r, err := e.compileTemp(id, spec)
	if err != nil {
		e.tempSeq--
		return "", err
	}
	r.Temp.Created = t
	if spec.Attach {
		if sender == "" || e.deps.Hub == nil {
			return "", fmt.Errorf("attach needs a bus connection")
		}
		temp := r.Temp
		temp.Owner = sender
		cancel, err := e.deps.Hub.WatchOwner(bus.Session, sender, func(newOwner string) {
			if newOwner == "" {
				e.post(func() {
					if !temp.gone {
						slog.Info("caller exited; dropping temporary rule", "rule", r.Def.Name)
						temp.gone = true
						e.dirty = true
					}
				})
			}
		})
		if err != nil {
			return "", err
		}
		temp.unwatch = cancel
		if !e.deps.Hub.NameHasOwner(bus.Session, sender) {
			cancel()
			return "", fmt.Errorf("caller already exited")
		}
	}
	e.temps = append(e.temps, r)
	slog.Info("added temporary rule", "rule", r.Def.Name, "while", r.Def.While.Source, "until", spec.Until)
	e.dirty = true
	return id, nil
}

// EnsureNow adds a temporary rule and applies it before returning, so the
// caller knows the outputs are in place. problems lists outputs the rule
// wants that could not be applied (they are retried). It must run on the
// engine goroutine.
//
// waiting is set when the rule's condition cannot be evaluated yet (a fact
// has no confirmed value); the rule then applies as soon as it can. That is
// not a problem: a false condition is not one either.
func (e *Engine) EnsureNow(spec TempSpec, sender string) (id string, problems []string, waiting string, err error) {
	id, err = e.Ensure(spec, sender)
	if err != nil {
		return "", nil, "", err
	}
	if !e.settled {
		// An explicit request should not wait for slow sources.
		e.settled = true
	}
	e.dirty = false
	e.Evaluate(now())
	var rule *Rule
	for _, r := range e.temps {
		if r.Temp.ID == id {
			rule = r
		}
	}
	if rule == nil {
		return id, nil, "", nil
	}
	if rule.Waiting {
		waiting = rule.Err
	} else if rule.Err != "" {
		problems = append(problems, "condition: "+rule.Err)
	}
	for _, o := range e.outputs {
		if _, ok := rule.Def.Outputs[o.Name()]; !ok {
			continue
		}
		for _, it := range o.Status() {
			wanted := false
			for _, w := range it.WantedBy {
				wanted = wanted || w == rule.Def.Name
			}
			if wanted && !it.Held {
				msg := o.Name() + " " + it.Key + " is not held"
				if it.Err != "" {
					msg += ": " + it.Err
				}
				problems = append(problems, msg)
			}
		}
	}
	return id, problems, waiting, nil
}

// Release drops temporary rules: the one with the given id or name, or all of
// them when id is empty. It returns how many were dropped.
func (e *Engine) Release(id string) int {
	n := 0
	for _, r := range e.temps {
		if r.Temp.gone {
			continue
		}
		if id == "" || r.Temp.ID == id || r.Def.Name == id || r.Def.Name == "ensure:"+id {
			r.Temp.gone = true
			n++
		}
	}
	if n > 0 {
		e.dirty = true
	}
	return n
}

// dropFinishedTemps forgets temporary rules that have ended and no longer
// match (so their outputs were already released).
func (e *Engine) dropFinishedTemps(t time.Time) {
	kept := e.temps[:0]
	for _, r := range e.temps {
		if r.Temp.finished(t) && !r.Matched {
			if r.Temp.unwatch != nil {
				r.Temp.unwatch()
			}
			slog.Info("temporary rule ended", "rule", r.Def.Name)
			continue
		}
		kept = append(kept, r)
	}
	for i := len(kept); i < len(e.temps); i++ {
		e.temps[i] = nil
	}
	e.temps = kept
}
