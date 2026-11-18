package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/PeterSR/systemd-whilst/internal/config"
	"github.com/PeterSR/systemd-whilst/internal/engine"
)

const unitName = "whilst.service"

// unitFile renders the systemd user unit that runs bin.
func unitFile(bin string) string {
	return `[Unit]
Description=whilst: keep the machine in the state its rules ask for
Documentation=https://github.com/PeterSR/systemd-whilst
After=dbus.socket

[Service]
Type=dbus
BusName=io.github.PeterSR.Whilst
ExecStart=` + systemdQuote(bin) + ` run
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`
}

// systemdQuote quotes a path for an Exec line. Specifiers (%) are escaped so
// the path is taken literally. systemd does not expand $ in the program path.
// Paths it cannot run at all are refused by checkUnitPath first.
func systemdQuote(s string) string {
	return `"` + strings.ReplaceAll(s, "%", "%%") + `"`
}

// checkUnitPath refuses paths systemd will not accept as an executable.
func checkUnitPath(p string) error {
	if strings.ContainsAny(p, "\"\\") || strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("systemd cannot run a binary at %q (quotes, backslashes and control characters are not allowed); move it and run install again", p)
	}
	return nil
}

func unitDir() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user"), nil
}

// localBin is where install copies the binary when it is not already in a
// directory on PATH.
func localBin() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(p) {
			continue
		}
		if real, err := filepath.EvalSymlinks(p); err == nil && real == dir {
			return true
		}
	}
	return false
}

// selfPath is the absolute, symlink-free path of the running binary.
func selfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// copyFile copies src to dst through a temporary file in dst's directory and
// a rename, so a running copy of dst is replaced, not overwritten.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".whilst-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// cmdInstall installs the binary and the user service, and (re)starts it.
// Running it again after downloading a new version upgrades in place.
func cmdInstall(args []string, w io.Writer) error {
	fs := newFlags("install")
	if pos, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usagef("install takes no arguments")
	}
	self, err := selfPath()
	if err != nil {
		return fmt.Errorf("finding this binary: %w", err)
	}
	bin := self
	if filepath.Base(self) != "whilst" || !onPath(filepath.Dir(self)) {
		dir, err := localBin()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			dir = real
		}
		bin = filepath.Join(dir, "whilst")
		if err := copyFile(self, bin); err != nil {
			return fmt.Errorf("copying to %s: %w", bin, err)
		}
		fmt.Fprintf(w, "installed %s\n", bin)
		if !onPath(dir) {
			fmt.Fprintf(w, "note: %s is not on your PATH; add it to run whilst by name\n", dir)
		}
	} else {
		fmt.Fprintf(w, "using %s (already on PATH)\n", bin)
	}

	if err := checkUnitPath(bin); err != nil {
		return err
	}
	if p, err := exec.LookPath("whilst"); err == nil {
		if real, err := filepath.EvalSymlinks(p); err == nil && real != bin {
			fmt.Fprintf(w, "note: `whilst` on your PATH is %s, which comes before %s; remove one of them\n", real, bin)
		}
	}

	dir, err := unitDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unit := filepath.Join(dir, unitName)
	if err := os.WriteFile(unit, []byte(unitFile(bin)), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(w, "wrote %s\n", unit)
	for _, a := range [][]string{{"daemon-reload"}, {"enable", unitName}} {
		if err := systemctl(a...); err != nil {
			return err
		}
	}
	if err := systemctl("restart", unitName); err != nil {
		return fmt.Errorf("%w\nThe service did not start. Run 'whilst check' and 'systemctl --user status whilst', or 'whilst doctor'", err)
	}
	fmt.Fprintln(w, "whilst.service is enabled and running")

	cfg := config.DefaultPath()
	if _, err := os.Stat(cfg); err != nil {
		fmt.Fprintf(w, "\nNo config yet. Write %s (see the README for examples),\nthen run 'whilst check'. The service picks it up by itself.\n", cfg)
	}
	return nil
}

// cmdUninstall stops and removes the user service. It leaves the binary,
// config and state alone and says where they are.
func cmdUninstall(args []string, w io.Writer) error {
	fs := newFlags("uninstall")
	if pos, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usagef("uninstall takes no arguments")
	}
	dir, err := unitDir()
	if err != nil {
		return err
	}
	unit := filepath.Join(dir, unitName)
	bin := ""
	if b, err := os.ReadFile(unit); err == nil {
		bin = unitBinary(string(b))
	}
	if _, err := os.Stat(unit); err == nil {
		// Disabling an already stopped or disabled unit is fine; a failure
		// here should not keep the unit file around.
		_ = systemctl("disable", "--now", unitName)
		if err := os.Remove(unit); err != nil {
			return err
		}
		fmt.Fprintf(w, "removed %s\n", unit)
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(w, "%s is not installed\n", unit)
	}
	if bin != "" {
		fmt.Fprintf(w, "left in place: the binary %s, ", bin)
	} else {
		fmt.Fprint(w, "left in place: ")
	}
	fmt.Fprintf(w, "config in %s, state in %s\n", filepath.Dir(config.DefaultPath()), filepath.Dir(engine.DefaultStatePath()))
	return nil
}

// unitBinary returns the binary path from a unit written by unitFile, or ""
// if it cannot tell.
func unitBinary(unit string) string {
	for _, line := range strings.Split(unit, "\n") {
		rest, ok := strings.CutPrefix(line, `ExecStart="`)
		if !ok {
			continue
		}
		path, ok := strings.CutSuffix(rest, `" run`)
		if !ok {
			return ""
		}
		return strings.ReplaceAll(path, "%%", "%")
	}
	return ""
}
