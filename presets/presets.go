// Package presets embeds the preset fact files that a config can include with
// include = ["preset:<name>"]. They are ordinary config, kept here so they
// ship inside the binary.
package presets

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.toml
var Files embed.FS

// Names lists the available presets.
func Names() []string {
	entries, _ := fs.ReadDir(Files, ".")
	var out []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".toml"); ok {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
