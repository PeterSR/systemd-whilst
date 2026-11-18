package systemd_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PeterSR/systemd-whilst/systemd/ipc"
	"github.com/PeterSR/whilst/engine"
	"github.com/PeterSR/whilst/plugin"
)

// testdata/api-v0.4.0.jsonl holds the JSON that whilst v0.4.0 (before the
// core and the application were split) produced for the values below, one
// per line: a status snapshot with every field set, an empty one, three
// temporary rule specs and two Ensure replies. These are what the D-Bus API
// carries (Status, Ensure and its reply), and what `whilst status --json`
// prints, so the JSON must not change. Every field of these types existed in
// v0.4.0, so all of them are pinned.
func TestAPIJSONFromV040(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	full := engine.Status{
		Config: "/home/u/.config/whilst/config.toml", ConfigError: "bad config",
		Started: t0, Now: t0.Add(time.Hour), NextWake: t0.Add(90 * time.Minute), Settled: true,
		Facts: []engine.FactStatus{
			{Name: "ac", Kind: "dbus", Source: "system org.freedesktop.UPower", Value: true, HasValue: true, Provisional: true,
				Changed: t0.Add(time.Minute), Updated: t0.Add(2 * time.Minute), Error: "cannot read", Pending: false, PendingDue: t0.Add(3 * time.Minute)},
			{Name: "battery", Kind: "command", Value: float64(41.5), HasValue: true},
			{Name: "n", Kind: "state", Value: int64(3), HasValue: true},
			{Name: "l", Kind: "expr", Source: "[1, 'a']", Value: []any{int64(1), "a", nil}, HasValue: true},
			{Name: "m", Kind: "expr", Value: map[string]any{"k": "v", "a": int64(1)}, HasValue: true},
			{Name: "none", Kind: "dbus_signal", Value: nil},
		},
		Rules: []engine.RuleStatus{
			{Name: "ensure:build", Origin: "command line", While: "true", Priority: 1 << 30, Matched: true, Since: t0,
				Outputs: map[string]any{"inhibit": []string{"sleep", "idle"}}, TempID: "t3", Until: t0.Add(2 * time.Hour), Attached: true},
			{Name: "rule-ab12cd34", Origin: "/home/u/.config/whilst/config.toml", While: "ac && battery > 20", Hold: "battery > 10", Holding: true,
				Priority: 5, Matched: true, Since: t0.Add(time.Minute), Error: "waiting for a first reading of ac",
				Facts: []string{"ac", "battery"}, Outputs: map[string]any{"units": []string{"backup.service"}},
				Enter: []string{"notify", "set"}, Exit: []string{"command", "dbus_call"}, LingerUntil: t0.Add(5 * time.Minute)},
			{Name: "plain", While: "false"},
		},
		Outputs: []engine.OutputStatus{
			{Output: "inhibit", Items: []plugin.Item{
				{Key: "sleep", Held: true, WantedBy: []string{"ensure:build"}, Note: "fd 7", Err: "denied"},
				{Key: "idle"},
			}},
			{Output: "units", Items: nil},
		},
		Warnings: []string{"rule x depends on the clock"},
	}
	specs := []engine.TempSpec{
		{Name: "build", While: "ac", Until: t0.Add(time.Hour), Outputs: map[string]any{"inhibit": []any{"sleep"}, "units": "x.service"}, Attach: true},
		{Outputs: map[string]any{"inhibit": "sleep"}},
		{},
	}
	res := []ipc.EnsureResult{
		{ID: "t1", Problems: []string{"inhibit sleep is not held: denied"}, Waiting: "waiting for a first reading of ac"},
		{ID: "t2"},
	}

	var got bytes.Buffer
	for _, v := range []any{full, engine.Status{}, specs, res} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		got.Write(b)
		got.WriteByte('\n')
	}
	want, err := os.ReadFile(filepath.Join("testdata", "api-v0.4.0.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	gl, wl := bytes.Split(got.Bytes(), []byte("\n")), bytes.Split(want, []byte("\n"))
	if len(gl) != len(wl) {
		t.Fatalf("got %d lines, want %d:\n%s", len(gl), len(wl), got.Bytes())
	}
	for i := range wl {
		if !bytes.Equal(gl[i], wl[i]) {
			t.Errorf("line %d differs from v0.4.0:\n got: %s\nwant: %s", i+1, gl[i], wl[i])
		}
	}

	// The JSON must also read back into the same values the client uses.
	var st engine.Status
	if err := json.Unmarshal(wl[0], &st); err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(st); !bytes.Equal(b, wl[0]) {
		t.Errorf("status does not round-trip:\n got: %s\nwant: %s", b, wl[0])
	}
}
