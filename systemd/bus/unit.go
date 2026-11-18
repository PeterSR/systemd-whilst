package bus

import (
	"fmt"
	"strings"
)

// ValidUnitName does a light check of a systemd unit name.
func ValidUnitName(name string) error {
	if name == "" || strings.ContainsAny(name, "/ \t\n") {
		return fmt.Errorf("%q is not a valid unit name", name)
	}
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return fmt.Errorf("%q needs a unit type suffix, like .service or .timer", name)
	}
	switch name[dot+1:] {
	case "service", "socket", "target", "timer", "path", "mount", "automount", "swap", "slice", "scope", "device":
		return nil
	}
	return fmt.Errorf("%q has an unknown unit type", name)
}
