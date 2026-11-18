package main

import (
	"strings"
	"testing"
)

func TestUnitFileQuotesExecStart(t *testing.T) {
	u := unitFile(`/home/a b/100%/$x/whilst`)
	want := `ExecStart="/home/a b/100%%/$x/whilst" run`
	if !strings.Contains(u, want+"\n") {
		t.Fatalf("unit lacks %q:\n%s", want, u)
	}
	if got := unitBinary(u); got != `/home/a b/100%/$x/whilst` {
		t.Errorf("unitBinary = %q", got)
	}
	for _, bad := range []string{`/a"b/whilst`, `/a\b/whilst`, "/a\nb/whilst"} {
		if checkUnitPath(bad) == nil {
			t.Errorf("checkUnitPath(%q) accepted", bad)
		}
	}
	for _, s := range []string{"Type=dbus", "BusName=io.github.PeterSR.Whilst", "WantedBy=default.target"} {
		if !strings.Contains(u, s) {
			t.Errorf("unit lacks %q", s)
		}
	}
}
