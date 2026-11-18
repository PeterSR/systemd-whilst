// Package output implements the level-triggered outputs that rules keep true
// while they match: inhibitor locks and running systemd units.
package output

import (
	"sort"

	"github.com/PeterSR/whilst/plugin"
)

// union collects the claimed strings with the rules that want each one.
func union(wants []plugin.Want) map[string][]string {
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
