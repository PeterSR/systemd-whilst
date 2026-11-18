package source

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PeterSR/systemd-whilst/systemd/bus"
	"github.com/PeterSR/whilst/value"
)

func TestCommandOutputs(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		run, output string
		want        any
		wantErr     bool
	}{
		{"echo '  hi '", "stdout", "hi", false},
		{"exit 3", "stdout", nil, true},
		{"exit 3", "status", false, false},
		{"true", "status", true, false},
		{`echo '{"a": [1, 2]}'`, "json", map[string]any{"a": []any{int64(1), int64(2)}}, false},
	}
	for _, c := range cases {
		cmd := &command{cfg: Command{Run: c.run, Output: c.output, Timeout: 5 * time.Second}}
		v, err := cmd.read(ctx)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: expected an error", c.run)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.run, err)
			continue
		}
		if got := bus.Value(v); !equal(got, c.want) {
			t.Errorf("%s: got %#v want %#v", c.run, got, c.want)
		}
	}
}

func TestCommandTimeoutKillsGroup(t *testing.T) {
	start := time.Now()
	_, _, _, err := RunShell(context.Background(), "sleep 10 & sleep 10", 200*time.Millisecond, nil, true)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not kill the process group")
	}
}

func equal(a, b any) bool { return value.Equal(a, b) }

func TestBackgroundJobDoesNotBlock(t *testing.T) {
	start := time.Now()
	// Without capture, `app &` returns when the shell exits.
	_, _, code, err := RunShell(context.Background(), "sleep 5 &", 10*time.Second, nil, false)
	if err != nil || code != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("uncaptured background job blocked: %v %d %s", err, code, time.Since(start))
	}
	// With capture, an escaped child holding stdout is cut off soon after.
	start = time.Now()
	out, _, _, err := RunShell(context.Background(), "setsid sleep 8 & echo hi", time.Minute, nil, true)
	if err != nil || strings.TrimSpace(string(out)) != "hi" || time.Since(start) > 5*time.Second {
		t.Fatalf("captured: %q %v %s", out, err, time.Since(start))
	}
}
