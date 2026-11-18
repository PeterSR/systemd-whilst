// Package output implements the level-triggered outputs that rules keep true
// while they match: inhibitor locks and running systemd units.
package output

import (
	"encoding/json"
	"sort"
)

// Want is one matching rule's claim on an output.
type Want struct {
	Rule  string
	Claim any // as returned by Parse
}

// Item is one thing an output manages, for status.
type Item struct {
	Key      string   `json:"key"`
	Held     bool     `json:"held"`
	WantedBy []string `json:"wanted_by,omitempty"`
	Note     string   `json:"note,omitempty"`
	Err      string   `json:"error,omitempty"`
}

// Output is one output type, named by the rule key that configures it.
type Output interface {
	// Name is the rule key, such as "inhibit".
	Name() string
	// Parse validates a rule's value for this output at config load.
	Parse(raw any) (any, error)
	// Reconcile makes the held state match the union of wants. It is called
	// after every evaluation, so it must be cheap when nothing changed. An
	// error means some part could not be applied; it is retried later.
	Reconcile(wants []Want) error
	// Status lists the managed items.
	Status() []Item
	// Save returns state to persist across restarts; Load restores it.
	Save() json.RawMessage
	Load(json.RawMessage) error
	// Close releases what can be released on shutdown.
	Close()
}

// union collects the claimed strings with the rules that want each one.
func union(wants []Want) map[string][]string {
	out := map[string][]string{}
	for _, w := range wants {
		for _, s := range w.Claim.([]string) {
			out[s] = append(out[s], w.Rule)
		}
	}
	for _, rules := range out {
		sort.Strings(rules)
	}
	return out
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
