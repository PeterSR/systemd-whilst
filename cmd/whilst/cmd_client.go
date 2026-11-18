package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/PeterSR/systemd-whilst/internal/config"
	"github.com/PeterSR/systemd-whilst/internal/engine"
	"github.com/PeterSR/systemd-whilst/internal/ipc"
	"github.com/PeterSR/systemd-whilst/internal/output"
	"github.com/PeterSR/systemd-whilst/internal/value"
)

func dial() (*ipc.Client, error) { return ipc.Dial() }

func cmdStatus(args []string) error {
	fs := newFlags("status")
	asJSON := fs.Bool("json", false, "print JSON")
	if pos, err := parseFlags(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usagef("status takes no arguments")
	}
	cl, err := dial()
	if err != nil {
		return err
	}
	defer cl.Close()
	st, err := cl.Status()
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	printStatus(os.Stdout, st)
	return nil
}

func ago(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "in " + roundDur(-d)
	case d < time.Second:
		return "just now"
	}
	return roundDur(d) + " ago"
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	case d < 48*time.Hour:
		return d.Round(time.Minute).String()
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func fmtValue(v any, has bool) string {
	if !has {
		return "(no value)"
	}
	s := value.Format(value.FromJSON(v))
	if len(s) > 40 {
		s = s[:37] + "..."
	}
	return s
}

func printStatus(w io.Writer, st *engine.Status) {
	now := st.Now
	fmt.Fprintf(w, "config   %s\n", st.Config)
	if st.ConfigError != "" {
		fmt.Fprintf(w, "         last reload failed: %s\n", st.ConfigError)
	}
	fmt.Fprintf(w, "running  since %s", st.Started.Local().Format("2006-01-02 15:04"))
	if !st.NextWake.IsZero() {
		fmt.Fprintf(w, ", next timed check %s", ago(st.NextWake, now))
	}
	if !st.Settled {
		fmt.Fprint(w, ", still waiting for facts")
	}
	fmt.Fprintln(w)
	for _, warn := range st.Warnings {
		fmt.Fprintf(w, "warning  %s\n", warn)
	}

	fmt.Fprintln(w, "\nFacts")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(st.Facts) == 0 {
		fmt.Fprintln(tw, "  (none)")
	}
	for _, f := range st.Facts {
		note := ""
		if f.Changed.IsZero() {
			note = "unchanged since start"
		} else {
			note = "changed " + ago(f.Changed, now)
		}
		if f.Provisional {
			note = "not yet confirmed"
		}
		if f.Pending != nil {
			note += fmt.Sprintf(", becoming %s %s", fmtValue(f.Pending, true), ago(f.PendingDue, now))
		}
		if f.Error != "" {
			note += "  ERROR: " + f.Error
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", f.Name, fmtValue(f.Value, f.HasValue), note)
	}
	tw.Flush()

	fmt.Fprintln(w, "\nRules")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(st.Rules) == 0 {
		fmt.Fprintln(tw, "  (none)")
	}
	for _, r := range st.Rules {
		mark := "-"
		if r.Matched {
			mark = "*"
		}
		extra := ""
		if r.TempID != "" {
			extra = " [" + r.TempID
			if !r.Until.IsZero() {
				extra += ", ends " + ago(r.Until, now)
			}
			if r.Attached {
				extra += ", attached"
			}
			extra += "]"
		}
		since := ""
		if !r.Since.IsZero() {
			since = ago(r.Since, now)
		}
		if !r.LingerUntil.IsZero() {
			extra += " [lingering, ends " + ago(r.LingerUntil, now) + "]"
		}
		line := fmt.Sprintf("  %s %s%s\t%s\t%s\t%s", mark, r.Name, extra, r.While, since, describeOutputs(r.Outputs))
		if r.Error != "" {
			line += "  ERROR: " + r.Error
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()

	fmt.Fprintln(w, "\nOutputs")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	anyHeld := false
	for _, o := range st.Outputs {
		for _, it := range o.Items {
			anyHeld = true
			held := "released"
			if it.Held {
				held = "held"
			}
			by := ""
			if len(it.WantedBy) > 0 {
				by = "wanted by " + strings.Join(it.WantedBy, ", ")
			}
			line := fmt.Sprintf("  %s\t%s\t%s\t%s", o.Output, it.Key, held, by)
			if it.Note != "" {
				line += " (" + it.Note + ")"
			}
			if it.Err != "" {
				line += "  ERROR: " + it.Err
			}
			fmt.Fprintln(tw, line)
		}
	}
	if !anyHeld {
		fmt.Fprintln(tw, "  (nothing held)")
	}
	tw.Flush()
}

func describeOutputs(outs map[string]any) string {
	keys := make([]string, 0, len(outs))
	for k := range outs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		var items []string
		if l, ok := outs[k].([]any); ok {
			for _, x := range l {
				items = append(items, fmt.Sprint(x))
			}
		} else {
			items = append(items, fmt.Sprint(outs[k]))
		}
		parts = append(parts, k+"="+strings.Join(items, ","))
	}
	return strings.Join(parts, " ")
}

func cmdWhy(args []string) error {
	if len(args) != 1 {
		return usagef("why takes one target: an output, an item or a rule name")
	}
	target := args[0]
	cl, err := dial()
	if err != nil {
		return err
	}
	defer cl.Close()
	st, err := cl.Status()
	if err != nil {
		return err
	}
	return explain(os.Stdout, st, target)
}

func explain(w io.Writer, st *engine.Status, target string) error {
	facts := map[string]engine.FactStatus{}
	for _, f := range st.Facts {
		facts[f.Name] = f
	}
	explainRule := func(r engine.RuleStatus, indent string) {
		state := "does not match"
		if r.Matched {
			state = "matches"
		}
		fmt.Fprintf(w, "%srule %s %s", indent, r.Name, state)
		if !r.Since.IsZero() {
			fmt.Fprintf(w, " (since %s)", ago(r.Since, st.Now))
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%s  while %s\n", indent, r.While)
		if r.Error != "" {
			fmt.Fprintf(w, "%s  cannot evaluate: %s\n", indent, r.Error)
		}
		for _, name := range r.Facts {
			f, ok := facts[name]
			if !ok {
				continue
			}
			line := fmt.Sprintf("%s  %s = %s", indent, name, fmtValue(f.Value, f.HasValue))
			if !f.Changed.IsZero() {
				line += ", changed " + ago(f.Changed, st.Now)
			}
			if f.Error != "" {
				line += ", ERROR: " + f.Error
			}
			fmt.Fprintln(w, line)
		}
		if !r.Until.IsZero() {
			fmt.Fprintf(w, "%s  temporary, ends %s\n", indent, ago(r.Until, st.Now))
		}
		if !r.LingerUntil.IsZero() {
			fmt.Fprintf(w, "%s  condition is false; lingering, ends %s\n", indent, ago(r.LingerUntil, st.Now))
		}
	}

	for _, r := range st.Rules {
		if r.Name == target || r.Name == "ensure:"+target || r.TempID == target {
			explainRule(r, "")
			if len(r.Outputs) > 0 {
				fmt.Fprintf(w, "  outputs %s\n", describeOutputs(r.Outputs))
			}
			return nil
		}
	}

	outName, key, hasKey := strings.Cut(target, ":")
	found := false
	for _, o := range st.Outputs {
		known := map[string]bool{}
		for _, it := range o.Items {
			known[it.Key] = true
		}
		var keys []string
		switch {
		case hasKey && o.Output == outName:
			keys = []string{key}
		case !hasKey && o.Output == target:
			for _, it := range o.Items {
				keys = append(keys, it.Key)
			}
			if len(keys) == 0 {
				found = true
				fmt.Fprintf(w, "%s: nothing is held and no matching rule wants anything\n", o.Output)
				listCandidates(w, st, o.Output, "", explainRule)
			}
		case !hasKey:
			if known[target] {
				keys = []string{target}
			} else {
				// A key that no rule currently wants, but some rule could.
				for _, r := range st.Rules {
					if outputMentions(r.Outputs[o.Output], target) {
						keys = []string{target}
						break
					}
				}
			}
		}
		for _, k := range keys {
			found = true
			var item *output.Item
			for i := range o.Items {
				if o.Items[i].Key == k {
					item = &o.Items[i]
				}
			}
			switch {
			case item == nil:
				fmt.Fprintf(w, "%s %s: not held; no matching rule wants it\n", o.Output, k)
			case item.Held:
				fmt.Fprintf(w, "%s %s: held, wanted by %s\n", o.Output, k, strings.Join(item.WantedBy, ", "))
			case len(item.WantedBy) > 0:
				fmt.Fprintf(w, "%s %s: wanted by %s but NOT held\n", o.Output, k, strings.Join(item.WantedBy, ", "))
			default:
				fmt.Fprintf(w, "%s %s: not held\n", o.Output, k)
			}
			if item != nil && item.Note != "" {
				fmt.Fprintf(w, "  %s\n", item.Note)
			}
			if item != nil && item.Err != "" {
				fmt.Fprintf(w, "  error: %s\n", item.Err)
			}
			listCandidates(w, st, o.Output, k, explainRule)
		}
	}
	if !found {
		return fmt.Errorf("%q is not a rule, an output or anything an output manages", target)
	}
	return nil
}

func outputMentions(claim any, key string) bool {
	if l, ok := claim.([]any); ok {
		for _, x := range l {
			if fmt.Sprint(x) == key {
				return true
			}
		}
	}
	return false
}

func listCandidates(w io.Writer, st *engine.Status, out, key string, explainRule func(engine.RuleStatus, string)) {
	var rules []engine.RuleStatus
	for _, r := range st.Rules {
		claim, ok := r.Outputs[out]
		if !ok {
			continue
		}
		if key == "" || outputMentions(claim, key) {
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 {
		fmt.Fprintln(w, "  no rule asks for it")
		return
	}
	fmt.Fprintln(w, "\nRules that ask for it:")
	for _, r := range rules {
		explainRule(r, "  ")
	}
}

func cmdEnsure(args []string) (int, error) {
	var cmdArgs []string
	for i, a := range args {
		if a == "--" {
			args, cmdArgs = args[:i], args[i+1:]
			break
		}
	}
	fs := newFlags("ensure")
	while := fs.String("while", "", "CEL condition")
	until := fs.String("until", "", "end time (15:04, 2006-01-02T15:04 or 2006-01-02 15:04)")
	name := fs.String("name", "", "rule name")
	pid := fs.Int("pid", 0, "keep the rule while this process lives")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 0, err
	}
	spec := engine.TempSpec{Name: *name, While: *while, Outputs: map[string]any{}}
	var dur time.Duration
	for _, p := range pos {
		if k, v, ok := strings.Cut(p, "="); ok {
			if k == "" || v == "" {
				return 0, usagef("%q: expected OUTPUT=VALUE[,VALUE...]", p)
			}
			var list []any
			if prev, ok := spec.Outputs[k].([]any); ok {
				list = prev
			}
			for _, x := range strings.Split(v, ",") {
				if x = strings.TrimSpace(x); x != "" {
					list = append(list, x)
				}
			}
			spec.Outputs[k] = list
			continue
		}
		if dur != 0 {
			return 0, usagef("unexpected argument %q", p)
		}
		d, err := config.ParseDuration(p)
		if err != nil || d <= 0 {
			return 0, usagef("%q is neither a duration nor OUTPUT=VALUE", p)
		}
		dur = d
	}
	if len(spec.Outputs) == 0 {
		return 0, usagef("name at least one output, like inhibit=sleep")
	}
	if dur != 0 && *until != "" {
		return 0, usagef("give a duration or --until, not both")
	}
	if dur != 0 {
		spec.Until = time.Now().Add(dur)
	}
	if *until != "" {
		t, err := parseUntil(*until, time.Now())
		if err != nil {
			return 0, usagef("%v", err)
		}
		spec.Until = t
	}
	if *pid != 0 && len(cmdArgs) > 0 {
		return 0, usagef("give --pid or a command, not both")
	}
	if *pid != 0 {
		if err := syscall.Kill(*pid, 0); err != nil && err != syscall.EPERM {
			return 0, fmt.Errorf("process %d: %w", *pid, err)
		}
	}
	spec.Attach = *pid != 0 || len(cmdArgs) > 0

	cl, err := dial()
	if err != nil {
		return 0, err
	}
	defer cl.Close()
	res, err := cl.Ensure(spec)
	if err != nil {
		return 0, err
	}
	describe := describeSpec(spec)
	if res.Waiting != "" {
		fmt.Fprintf(os.Stderr, "whilst: the condition cannot be checked yet (%s); the rule applies once it can\n", res.Waiting)
	}
	if !spec.Attach {
		fmt.Printf("%s: %s\n", res.ID, describe)
		for _, p := range res.Problems {
			fmt.Fprintf(os.Stderr, "warning: %s (whilst keeps retrying)\n", p)
		}
		if spec.Until.IsZero() && spec.While == "" {
			fmt.Printf("held until `whilst release %s`\n", res.ID)
		}
		return 0, nil
	}
	if len(res.Problems) > 0 {
		cl.Release(res.ID)
		return 0, fmt.Errorf("could not apply the rule: %s", strings.Join(res.Problems, "; "))
	}

	// An attached rule lives on this process's bus connection. If whilst
	// restarts, add it again; until then, say that nothing is held.
	if err := cl.WatchRestarts(func(up bool) {
		if !up {
			fmt.Fprintln(os.Stderr, "whilst: the whilst service stopped; nothing is held until it is back")
			return
		}
		if r, err := cl.Ensure(spec); err != nil {
			fmt.Fprintf(os.Stderr, "whilst: the whilst service restarted but the rule could not be added again: %v\n", err)
		} else if len(r.Problems) > 0 {
			fmt.Fprintf(os.Stderr, "whilst: the whilst service restarted; rule added again with problems: %s\n", strings.Join(r.Problems, "; "))
		} else {
			fmt.Fprintln(os.Stderr, "whilst: the whilst service restarted; rule added again")
		}
	}); err != nil {
		fmt.Fprintf(os.Stderr, "whilst: warning: cannot watch for restarts of the service: %v\n", err)
	}

	if *pid != 0 {
		fmt.Fprintf(os.Stderr, "whilst: %s while process %d runs\n", describe, *pid)
		sigs := make(chan os.Signal, 1)
		notifyUnlessIgnored(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-sigs:
				return 130, nil
			case <-tick.C:
				if err := syscall.Kill(*pid, 0); err == syscall.ESRCH {
					return 0, nil
				}
			}
		}
	}

	fmt.Fprintf(os.Stderr, "whilst: %s while %s runs\n", describe, cmdArgs[0])
	c := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl-C reaches the child through the terminal; we exit when it does.
	// SIGINT and SIGQUIT are caught and dropped here, not ignored: an
	// ignored signal stays ignored in the child, a caught one does not.
	// Signals sent to this process alone (kill, timeout(1)) are passed on,
	// so the child never outlives the rule that covers it.
	tty := make(chan os.Signal, 4)
	notifyUnlessIgnored(tty, syscall.SIGINT, syscall.SIGQUIT)
	go func() {
		for range tty {
		}
	}()
	sigs := make(chan os.Signal, 4)
	notifyUnlessIgnored(sigs, syscall.SIGTERM, syscall.SIGHUP)
	if err := c.Start(); err != nil {
		return 127, err
	}
	go func() {
		for s := range sigs {
			c.Process.Signal(s)
		}
	}()
	if err := c.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal()), nil
			}
			return ee.ExitCode(), nil
		}
		return 127, err
	}
	return 0, nil
}

// notifyUnlessIgnored catches the signals that were not ignored when whilst
// started. A signal ignored by the caller (nohup, a background job in a
// script) must stay ignored, for us and for the command we run.
func notifyUnlessIgnored(ch chan os.Signal, sigs ...os.Signal) {
	for _, s := range sigs {
		if !signal.Ignored(s) {
			signal.Notify(ch, s)
		}
	}
}

func describeSpec(s engine.TempSpec) string {
	out := map[string]any{}
	for k, v := range s.Outputs {
		out[k] = v
	}
	d := "ensuring " + describeOutputs(out)
	if s.While != "" {
		d += " while " + s.While
	}
	if !s.Until.IsZero() {
		d += " until " + s.Until.Local().Format("2006-01-02 15:04:05")
	}
	return d
}

// parseUntil reads an end time: a time of day (today, or tomorrow if it has
// passed) or a date and time.
func parseUntil(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{"15:04", "15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local)
			if !at.After(now) {
				at = at.AddDate(0, 0, 1)
			}
			return at, nil
		}
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			if !t.After(now) {
				return time.Time{}, fmt.Errorf("%s is in the past", s)
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read %q as a time (use 15:04 or 2006-01-02 15:04)", s)
}

func cmdRelease(args []string) error {
	fs := newFlags("release")
	all := fs.Bool("all", false, "release every temporary rule")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	id := ""
	switch {
	case *all && len(pos) == 0:
	case !*all && len(pos) == 1:
		id = pos[0]
	default:
		return usagef("give one rule id or name, or --all")
	}
	cl, err := dial()
	if err != nil {
		return err
	}
	defer cl.Close()
	n, err := cl.Release(id)
	if err != nil {
		return err
	}
	if n == 0 {
		if id == "" {
			fmt.Println("no temporary rules")
			return nil
		}
		return fmt.Errorf("no temporary rule %q", id)
	}
	fmt.Printf("released %s\n", plural(n, "temporary rule"))
	return nil
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}
	return strconv.Itoa(n) + " " + s + "s"
}

func cmdReload(args []string) error {
	if len(args) > 0 {
		return usagef("reload takes no arguments")
	}
	cl, err := dial()
	if err != nil {
		return err
	}
	defer cl.Close()
	msg, err := cl.Reload()
	if err != nil {
		return err
	}
	fmt.Println(msg)
	return nil
}
