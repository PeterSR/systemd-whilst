package main

import (
	"strings"
	"testing"
	"time"

	"github.com/PeterSR/whilst/engine"
	"github.com/PeterSR/whilst/plugin"
)

func TestDescribeOutputsSystemUnits(t *testing.T) {
	got := describeOutputs(map[string]any{
		"units":   []any{"a.service", map[string]any{"name": "b.service", "bus": "system"}, map[string]any{"name": "c.service", "bus": "user"}},
		"inhibit": "sleep",
	})
	if got != "inhibit=sleep units=a.service,system:b.service,c.service" {
		t.Fatalf("got %q", got)
	}
}

func TestWhySystemUnit(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	st := &engine.Status{
		Now: now,
		Rules: []engine.RuleStatus{
			{Name: "sync", While: "home", Matched: true, Outputs: map[string]any{"units": []any{"b.service", map[string]any{"name": "b.service", "bus": "system"}}}},
		},
		Outputs: []engine.OutputStatus{{Output: "units", Items: []plugin.Item{
			{Key: "b.service", Held: true, WantedBy: []string{"sync"}, Note: "started by whilst"},
			{Key: "system:b.service", WantedBy: []string{"sync"}, Err: "not allowed: needs a polkit rule for this unit (denied)"},
		}}},
	}
	for _, target := range []string{"system:b.service", "units:system:b.service"} {
		var b strings.Builder
		if err := explain(&b, st, target); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		out := b.String()
		if !strings.HasPrefix(out, "units system:b.service: wanted by sync but NOT held\n  error: not allowed: needs a polkit rule for this unit (denied)\n") || !strings.Contains(out, "rule sync matches") {
			t.Fatalf("%s:\n%s", target, out)
		}
	}
	var b strings.Builder
	if err := explain(&b, st, "b.service"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "units b.service: held, wanted by sync\n  started by whilst\n") {
		t.Fatalf("user unit:\n%s", b.String())
	}
	if err := explain(&b, st, "nope:b.service"); err == nil {
		t.Fatal("expected an error for an unknown target")
	}
}
