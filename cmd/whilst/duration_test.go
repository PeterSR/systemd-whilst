package main

import (
	"testing"
	"time"
)

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"2h": 2 * time.Hour, "1.5": 90 * time.Minute, "45m": 45 * time.Minute, "2": 2 * time.Hour} {
		got, err := parseDuration(in)
		if err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	if _, err := parseDuration("soon"); err == nil {
		t.Error("expected error")
	}
}
