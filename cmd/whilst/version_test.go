package main

import (
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	info := func(v string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) { return &debug.BuildInfo{Main: debug.Module{Version: v}}, true }
	}
	none := func() (*debug.BuildInfo, bool) { return nil, false }
	for _, c := range []struct {
		stamped string
		read    func() (*debug.BuildInfo, bool)
		want    string
	}{
		{"v0.5.0", info("v0.4.0"), "v0.5.0"}, // a stamped version wins
		{"dev", info("v0.5.1"), "v0.5.1"},    // go install
		{"dev", info("(devel)"), "dev"},      // go run in a checkout
		{"dev", info(""), "dev"},
		{"dev", none, "dev"},
	} {
		if got := resolveVersion(c.stamped, c.read); got != c.want {
			t.Errorf("resolveVersion(%q) = %q, want %q", c.stamped, got, c.want)
		}
	}
}
