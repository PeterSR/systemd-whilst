package action

import (
	"strings"
	"testing"
)

// A container type in the signature is reported as unsupported, not as a
// mismatch between the number of types and arguments.
func TestConvertArgsRejectsContainers(t *testing.T) {
	for _, sig := range []string{"as", "(si)", "a{sv}"} {
		_, err := ConvertArgs(sig, []any{"x"})
		if err == nil || !strings.Contains(err.Error(), "only basic types") {
			t.Errorf("%s: got %v", sig, err)
		}
	}
	if _, err := ConvertArgs("sb", []any{"x", true}); err != nil {
		t.Errorf("basic types: %v", err)
	}
}
