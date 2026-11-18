package main

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/godbus/dbus/v5"

	"github.com/PeterSR/systemd-whilst/internal/bus"
	"github.com/PeterSR/systemd-whilst/internal/config"
	"github.com/PeterSR/systemd-whilst/internal/engine"
	"github.com/PeterSR/systemd-whilst/internal/ipc"
	"github.com/PeterSR/systemd-whilst/internal/output"
)

// doctor collects check results and prints them as it goes.
type doctor struct {
	w     io.Writer
	fails int
	warns int
}

func (d *doctor) ok(format string, a ...any)   { d.line("ok  ", format, a...) }
func (d *doctor) info(format string, a ...any) { d.line("    ", format, a...) }
func (d *doctor) warn(format string, a ...any) { d.warns++; d.line("warn", format, a...) }
func (d *doctor) fail(format string, a ...any) { d.fails++; d.line("FAIL", format, a...) }

func (d *doctor) line(tag, format string, a ...any) {
	fmt.Fprintf(d.w, "%s  %s\n", tag, fmt.Sprintf(format, a...))
}

func (d *doctor) section(name string) { fmt.Fprintf(d.w, "\n%s\n", name) }

// cmdDoctor checks the installation, the platform services whilst relies on,
// the config and the running service, and says what to do about problems.
func cmdDoctor(args []string, w io.Writer) error {
	fs := newFlags("doctor")
	if pos, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usagef("doctor takes no arguments")
	}
	d := &doctor{w: w}
	hub := bus.NewHub()
	defer hub.Close()

	fmt.Fprintf(w, "whilst %s\n", version)

	d.section("Buses")
	sessionOK := d.checkBus(hub, bus.Session, "session bus")
	systemOK := d.checkBus(hub, bus.System, "system bus")

	d.section("Installation")
	d.checkInstall(hub, sessionOK)

	d.section("logind")
	if systemOK {
		d.checkLogind(hub)
	} else {
		d.info("skipped: no system bus")
	}

	d.section("Config")
	d.checkConfig()

	d.section("Service")
	if sessionOK {
		d.checkService()
	} else {
		d.info("skipped: no session bus")
	}

	fmt.Fprintln(w)
	switch {
	case d.fails > 0:
		return fmt.Errorf("%d problem(s), %d warning(s)", d.fails, d.warns)
	case d.warns > 0:
		fmt.Fprintf(w, "no problems, %d warning(s)\n", d.warns)
	default:
		fmt.Fprintln(w, "no problems found")
	}
	return nil
}

func (d *doctor) checkBus(hub *bus.Hub, name, label string) bool {
	if _, err := hub.Conn(name); err != nil {
		d.fail("%s: %v", label, err)
		return false
	}
	d.ok("%s", label)
	return true
}

func (d *doctor) checkInstall(hub *bus.Hub, sessionOK bool) {
	onPathBin := ""
	if p, err := exec.LookPath("whilst"); err != nil {
		d.warn("`whilst` is not on PATH; run `whilst install`, or add ~/.local/bin to PATH")
	} else if real, err := filepath.EvalSymlinks(p); err == nil {
		onPathBin = real
	}

	dir, err := unitDir()
	if err != nil {
		d.fail("finding the user unit directory: %v", err)
		return
	}
	unit := filepath.Join(dir, unitName)
	raw, err := os.ReadFile(unit)
	if err != nil {
		d.warn("%s is not installed; run `whilst install`", unitName)
		return
	}
	bin := unitBinary(string(raw))
	switch {
	case bin == "":
		d.warn("%s does not look like one `whilst install` wrote; run `whilst install` to replace it", unit)
	default:
		if st, err := os.Stat(bin); err != nil {
			d.fail("the service runs %s, which is missing; run `whilst install`", bin)
		} else if st.Mode()&0o111 == 0 {
			d.fail("the service runs %s, which is not executable", bin)
		} else {
			d.ok("%s runs %s", unitName, bin)
			if real, err := filepath.EvalSymlinks(bin); err == nil && onPathBin != "" && onPathBin != real {
				d.warn("`whilst` on PATH is %s, but the service runs %s; they may be different versions", onPathBin, real)
			}
		}
	}
	if !sessionOK {
		return
	}
	c, err := hub.Conn(bus.Session)
	if err != nil {
		return
	}
	var path dbus.ObjectPath
	if err := bus.Call(c, "org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager.LoadUnit", unitName).Store(&path); err != nil {
		d.fail("systemd user manager: %v", err)
		return
	}
	prop := func(iface, name string) any {
		v, err := bus.GetProperty(c, "org.freedesktop.systemd1", string(path), iface, name)
		if err != nil {
			return nil
		}
		return v.Value()
	}
	if s, _ := prop("org.freedesktop.systemd1.Unit", "UnitFileState").(string); s != "enabled" {
		d.warn("%s is %s, so it will not start at login; run `whilst install`", unitName, orNone(s))
	} else {
		d.ok("%s is enabled", unitName)
	}
	state, _ := prop("org.freedesktop.systemd1.Unit", "ActiveState").(string)
	switch state {
	case "active":
	case "activating", "reloading", "deactivating":
		d.warn("%s is %s; run doctor again in a moment", unitName, state)
		return
	default:
		d.fail("%s is %s; see `journalctl --user -u whilst`", unitName, orNone(state))
		return
	}
	d.ok("%s is active", unitName)
	pid, _ := prop("org.freedesktop.systemd1.Service", "MainPID").(uint32)
	if pid == 0 {
		return
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return
	}
	if strings.HasSuffix(exe, " (deleted)") {
		d.warn("the service still runs a binary that has since been replaced; restart it with `systemctl --user restart whilst`")
	} else if bin != "" && exe != bin {
		if real, err := filepath.EvalSymlinks(bin); err != nil || real != exe {
			d.warn("the service runs %s, but the unit names %s; restart it with `systemctl --user restart whilst`", exe, bin)
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (d *doctor) checkLogind(hub *bus.Hub) {
	c, err := hub.Conn(bus.System)
	if err != nil {
		return
	}
	const dest, path, iface = "org.freedesktop.login1", "/org/freedesktop/login1", "org.freedesktop.login1.Manager"
	if _, err := bus.GetProperty(c, dest, path, iface, "IdleAction"); err != nil {
		d.fail("systemd-logind: %v", err)
		return
	}
	d.ok("systemd-logind")
	str := func(p string) string {
		v, err := bus.GetProperty(c, dest, path, iface, p)
		if err != nil {
			return ""
		}
		s, _ := v.Value().(string)
		return s
	}
	if battery := str("HandleLidSwitch"); battery != "" {
		// An empty value means "same as HandleLidSwitch".
		mains := cmp.Or(str("HandleLidSwitchExternalPower"), battery)
		docked := cmp.Or(str("HandleLidSwitchDocked"), "ignore")
		d.info("closing the lid: %s on battery, %s on mains, %s when docked", battery, mains, docked)
		if v, err := bus.GetProperty(c, dest, path, iface, "LidSwitchIgnoreInhibited"); err == nil {
			if ignore, _ := v.Value().(bool); ignore {
				d.info("LidSwitchIgnoreInhibited=yes: only a handle-lid-switch lock stops that, a sleep lock does not")
			} else {
				d.info("LidSwitchIgnoreInhibited=no: a sleep lock also stops lid suspend")
			}
		} else {
			// Not exposed on D-Bus by every logind version; yes is the default.
			d.info("only a handle-lid-switch lock stops that; a sleep lock does not (unless logind.conf sets LidSwitchIgnoreInhibited=no)")
		}
	}

	// Take and drop the locks whilst would take, to see that logind allows
	// them for this user.
	l := output.LogindLocker{Hub: hub}
	f, err := l.Inhibit("sleep:idle:handle-lid-switch", "whilst doctor", "checking permissions", "block")
	if err != nil {
		var de dbus.Error
		if errors.As(err, &de) && (strings.Contains(de.Name, "AccessDenied") || strings.HasSuffix(de.Name, "InteractiveAuthorizationRequired")) {
			d.fail("logind refused an inhibitor lock: %v. polkit allows it for an active local login session; it is refused over SSH or with no one logged in at the machine", err)
		} else {
			d.fail("taking an inhibitor lock: %v", err)
		}
		return
	}
	f.Close()
	d.ok("inhibitor locks (sleep, idle, handle-lid-switch) are allowed")
}

func (d *doctor) checkConfig() {
	path := config.DefaultPath()
	if _, err := os.Stat(path); err != nil {
		d.warn("no config at %s; whilst has no rules until you write one", path)
		return
	}
	cfg, err := config.Load(path)
	if err != nil {
		d.fail("%v", err)
		return
	}
	comp, err := engine.Compile(cfg, offlineOutputs())
	if err != nil {
		d.fail("%s: %v", path, err)
		return
	}
	d.ok("%s: %d facts and %d rules", path, len(comp.Facts), len(comp.Rules))
	for _, w := range comp.Warnings {
		d.warn("%s", w)
	}
}

func (d *doctor) checkService() {
	cl, err := ipc.Dial()
	if err != nil {
		d.fail("%v", err)
		return
	}
	defer cl.Close()
	st, err := cl.Status()
	if err != nil {
		d.fail("%v", err)
		return
	}
	d.ok("the service answers on the session bus")
	if st.ConfigError != "" {
		d.fail("the service could not load its config, and runs the last good one: %s", st.ConfigError)
	}
	if path := config.DefaultPath(); st.Config != "" && st.Config != path {
		d.info("the service uses %s, not %s", st.Config, path)
	}
	for _, w := range st.Warnings {
		d.warn("%s", w)
	}
	for _, f := range st.Facts {
		if f.Error != "" {
			d.warn("fact %s: %s", f.Name, f.Error)
		}
	}
	for _, r := range st.Rules {
		if r.Error != "" {
			d.warn("rule %s: %s", r.Name, r.Error)
		}
	}
	for _, o := range st.Outputs {
		for _, it := range o.Items {
			if it.Err != "" {
				d.warn("%s %s: %s", o.Output, it.Key, it.Err)
			}
		}
	}
	if !st.Settled {
		d.info("the service is still settling after start; facts may not be read yet")
	}
}
