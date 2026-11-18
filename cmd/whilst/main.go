// Command whilst keeps a Linux machine in the state its rules ask for, for as
// long as their conditions hold. See README.md and DESIGN.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/PeterSR/systemd-whilst/presets"
	"github.com/PeterSR/systemd-whilst/systemd"
	"github.com/PeterSR/whilst/config"
	"github.com/PeterSR/whilst/engine"
)

// version is set by the Makefile and release builds; other builds (such as
// go install) take it from the module version Go records in the binary.
var version = "dev"

func init() { version = resolveVersion(version, debug.ReadBuildInfo) }

func resolveVersion(stamped string, read func() (*debug.BuildInfo, bool)) string {
	if stamped != "dev" {
		return stamped
	}
	if info, ok := read(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return stamped
}

const usage = `whilst keeps your machine in the state your rules ask for, for as long as
their conditions hold.

Usage:
  whilst run [--config PATH] [--debug] [--no-watch]
                                         run the engine (what the user service starts)
  whilst status [--json]                 facts, rules and outputs right now
  whilst why TARGET                      why an output is (not) held, or why a rule
                                         does (not) match. TARGET is an output
                                         (inhibit), an item (inhibit:sleep, sleep,
                                         syncthing.service) or a rule name
  whilst check [PATH]                    load and type-check a config
  whilst ensure [DURATION] OUTPUT=VALUE... [flags] [-- COMMAND...]
                                         add a temporary top-priority rule
  whilst release ID | NAME | --all       drop temporary rules
  whilst reload                          re-read the config
  whilst presets [NAME]                  list the built-in presets, or print one
  whilst install                         install this binary and the user service,
                                         and (re)start it; run again to upgrade
  whilst uninstall                       stop and remove the user service
  whilst doctor                          check the setup and say what to fix
  whilst version

ensure:
  whilst ensure 2h inhibit=sleep                     for two hours
  whilst ensure --until 22:00 inhibit=sleep,idle     until a time of day
  whilst ensure inhibit=sleep -- make -j8            while a command runs
  whilst ensure inhibit=sleep --pid 4242             while a process lives
  whilst ensure --while 'ac' units=backup.service    while a condition holds
  Flags: --while CEL, --until TIME, --name NAME, --pid PID.
  Without a duration, --until, --pid or a command, the rule stays until
  'whilst release'.

Config: $WHILST_CONFIG or ~/.config/whilst/config.toml
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	code := 0
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "status":
		err = cmdStatus(args)
	case "why":
		err = cmdWhy(args)
	case "check":
		err = cmdCheck(args, os.Stdout)
	case "ensure":
		code, err = cmdEnsure(args)
	case "release":
		err = cmdRelease(args)
	case "reload":
		err = cmdReload(args)
	case "presets":
		err = cmdPresets(args)
	case "install":
		err = cmdInstall(args, os.Stdout)
	case "uninstall":
		err = cmdUninstall(args, os.Stdout)
	case "doctor":
		err = cmdDoctor(args, os.Stdout)
	case "version", "--version":
		fmt.Println("whilst", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "whilst: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "whilst %s: %v\n", cmd, err)
		var ue usageError
		if errors.As(err, &ue) {
			os.Exit(2)
		}
		os.Exit(1)
	}
	os.Exit(code)
}

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// newFlags returns a flag set whose errors are returned, not printed with
// the whole usage.
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseFlags parses flags anywhere among the arguments (the standard library
// stops at the first positional one).
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usagef("%v", err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func cmdCheck(args []string, w io.Writer) error {
	fs := newFlags("check")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	path := systemd.ConfigPath()
	switch len(pos) {
	case 0:
	case 1:
		path = pos[0]
	default:
		return usagef("check takes at most one path")
	}
	cfg, err := config.Load(path, systemd.Registry())
	if err != nil {
		return err
	}
	comp, err := engine.Compile(cfg, systemd.OfflineOutputs())
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s: OK, %d facts and %d rules\n", path, len(comp.Facts), len(comp.Rules))
	for _, warn := range comp.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
	return nil
}

func cmdPresets(args []string) error {
	switch len(args) {
	case 0:
		for _, n := range presets.Names() {
			fmt.Printf("preset:%s\n", n)
		}
		fmt.Println("\nInclude one with:  include = [\"preset:NAME\"]   (whilst presets NAME prints it)")
		return nil
	case 1:
		name := strings.TrimPrefix(args[0], "preset:")
		b, err := presets.Files.ReadFile(name + ".toml")
		if err != nil {
			return fmt.Errorf("no preset named %q", name)
		}
		os.Stdout.Write(b)
		return nil
	}
	return usagef("presets takes at most one name")
}
