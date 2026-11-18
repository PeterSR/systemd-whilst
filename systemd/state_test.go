package systemd_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PeterSR/whilst/value"
)

// stateConfig is goldenConfig with a named rule that lingers. A named rule
// leaves the names of the unnamed ones as they are.
const stateConfig = goldenConfig + `
[[rule]]
name = "lingering"
while = "p"
linger = "1h"
inhibit = "idle"
`

// testdata/state-v0.4.0.json was written by whilst v0.4.0 (before the core
// and the application were split) for stateConfig: values read at 12:00,
// two temporary rules added with ensure (one until a time, one with a
// condition), and p turned false at 12:01, so one rule matches through its
// hold condition and "lingering" is in its linger time. Within the same
// boot, every value must come back as a confirmed observation, rule, temp
// and unit state must be restored, and saving again must write the same
// file.
func TestStateFileFromV040(t *testing.T) {
	old, err := os.ReadFile("testdata/state-v0.4.0.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, stateConfig, path, "boot-golden")
	st := h.e.Snapshot()
	want := map[string]any{
		"c": map[string]any{"a": int64(1)}, "c2": "x", "e": true, "f": int64(1), "p": false,
		"s": "on", "s2": int64(4), "sig": false, "u1": "active", "u2": "inactive",
	}
	for _, f := range st.Facts {
		w, ok := want[f.Name]
		if !ok {
			// sig2 never had a value.
			if f.Name != "sig2" || f.HasValue {
				t.Errorf("unexpected fact %s = %v", f.Name, f.Value)
			}
			continue
		}
		if !f.HasValue || f.Provisional || !value.Equal(f.Value, w) {
			t.Errorf("fact %s: value %v (has %v, provisional %v), want %v", f.Name, f.Value, f.HasValue, f.Provisional, w)
		}
	}
	for _, f := range st.Facts {
		if f.Name == "p" && !f.Changed.Equal(time.Date(2026, 11, 18, 12, 1, 0, 0, time.UTC)) {
			t.Errorf("p changed %v", f.Changed)
		}
	}
	matched := map[string]bool{}
	for _, r := range st.Rules {
		matched[r.Name] = r.Matched
	}
	for name, want := range map[string]bool{
		"rule-b1d9c55e": true, "rule-91e4d4bd": true, "rule-e01cfa71": true, "rule-ca80ae7d": false, "rule-9ebec054": false, "rule-f642e73a": false, "rule-f642e73a-2": false,
		"lingering": true, "ensure:build": true, "ensure:t2": true,
	} {
		if m, ok := matched[name]; !ok || m != want {
			t.Errorf("rule %s: matched %v (known %v), want %v", name, m, ok, want)
		}
	}
	for _, o := range st.Outputs {
		if o.Output != "units" {
			continue
		}
		if len(o.Items) != 1 || o.Items[0].Key != "a.service" || o.Items[0].Note != "started by whilst" {
			t.Errorf("units %+v", o.Items)
		}
	}

	// Nothing changed, so an evaluation leaves the state as it was, and no
	// unit is started or stopped and no action runs. The file is
	// overwritten first, to see that the evaluation writes it again.
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.now = time.Date(2026, 11, 18, 12, 2, 0, 0, time.UTC)
	h.eval()

	// After the evaluation, rule-e01cfa71 matches through its hold
	// condition although its while (p) is false, "lingering" is still in
	// its linger time from 12:01, and both temporary rules are back.
	rules := map[string]engineRule{}
	for _, r := range h.e.Snapshot().Rules {
		rules[r.Name] = engineRule{r.Matched, r.Holding, r.LingerUntil, r.TempID, r.Until}
	}
	if r := rules["rule-e01cfa71"]; !r.matched || !r.holding {
		t.Errorf("rule-e01cfa71: matched %v, holding %v; want it to match through hold", r.matched, r.holding)
	}
	if r := rules["lingering"]; !r.matched || !r.lingerUntil.Equal(time.Date(2026, 11, 18, 13, 1, 0, 0, time.UTC)) {
		t.Errorf("lingering: matched %v, linger until %v", r.matched, r.lingerUntil)
	}
	if r := rules["ensure:build"]; !r.matched || r.tempID != "t1" || !r.until.Equal(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ensure:build: %+v", r)
	}
	if r := rules["ensure:t2"]; !r.matched || r.tempID != "t2" {
		t.Errorf("ensure:t2: %+v", r)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, old) {
		t.Errorf("state file changed on save:\n%s", got)
	}
	if len(h.systemd.started)+len(h.systemd.stopped)+len(h.actions.ran) != 0 {
		t.Errorf("started %v, stopped %v, actions %v", h.systemd.started, h.systemd.stopped, h.actions.ran)
	}
}

type engineRule struct {
	matched, holding bool
	lingerUntil      time.Time
	tempID           string
	until            time.Time
}
