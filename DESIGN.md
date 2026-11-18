# whilst: design

Status: implemented (v1). This records the model, the reasoning behind it, and the open questions. The README is the user-facing reference.

## Problem

Linux has plenty of tools that react to one kind of event (udev rules, NetworkManager dispatcher scripts, acpid, dbus-action, mplugd) and plenty that keep one area in shape (TLP, autorandr, kanshi, power-profiles-daemon). systemd's `Condition*=` checks run once, when a unit starts. Home Assistant with go-hass-agent can automate a laptop, but the logic lives on a server the laptop may not reach, and its automations fire on events rather than holding a state.

Nothing local and general says: "under these conditions, the machine should be like this, and when the conditions change, it should be like that instead".

The motivating case: block sleep including lid close at home on mains, keep the lid from suspending for 30 minutes after unplugging (to carry the laptop closed to another room), block idle sleep only when on mains elsewhere, and allow everything otherwise. systemd-block-sleep could express parts of this, one condition at a time. whilst generalises it.

## Model

Three concepts: **facts**, **rules** and **outputs**.

### Facts

A fact is a named, typed value plus the time it last changed. Nothing is built into the binary: every fact is declared in config. A fact needs two things, how to read its value and when to read it again, and each source type answers both:

| Source | Read | Re-read when |
|---|---|---|
| `dbus` property | `Get` | `PropertiesChanged`, subscribed automatically, and when the service restarts |
| `dbus_signal` | the signal payload | the signal arrives |
| `unit` | a systemd unit's `ActiveState` | `PropertiesChanged` on the unit (after `Subscribe` on the manager) |
| `command` | stdout, exit status or JSON | a trigger the config names: D-Bus match rules or an interval |
| `file` | file contents | polled (sysfs files often do not notify, so polling is the reliable choice) |
| `expr` | a CEL expression over other facts | any fact it uses changes |
| `state` | a value only rule actions can write | a rule sets it |

The clock is always available as `now`, and whilst schedules its own timers (see Time).

D-Bus properties are the preferred source: value and change notification come together, typed, with no polling and no process per read. A command gives a value but no change signal, so it always needs a trigger, and that trigger is usually a D-Bus signal anyway. Commands are the escape hatch for things not on the bus.

Every source that can be re-read also accepts the generic triggers `on_dbus`, `on_dbus_session` and `every`. Some D-Bus properties never announce changes (logind's `Docked` is one), and `every` covers them.

A fact can declare a `type`, which CEL uses to type-check rules, and a `transform`, a CEL expression over the raw `value` (for example `!value` to turn UPower's `OnBattery` into `ac`).

Common facts (UPower, logind, NetworkManager) ship as **presets**: plain config files embedded in the binary and included with `include = ["preset:upower"]`. They are a convenience, not built-in logic.

`expr` facts replace a separate "context" concept. Naming an expression is optional (it can be written inline in a rule), but a named one gets its own `since()` timestamp, shows up in `status`, and can be shared by many rules.

A source keeps its last good value when a read fails or times out, and the error shows in `status`. It does not fall back to a default. A fact can set `debounce`: a new value must hold that long before it counts, which absorbs blips such as a Wi-Fi reconnect.

The first value read for a fact is not a change. Otherwise starting whilst on battery would look like "just unplugged". More generally, a value that is only a guess (an `initial` value, or one remembered from before a reboot) is *provisional*: replacing it is not a change, an expr fact computed from it is provisional too, and a rule whose condition reads it keeps its current state until the fact has really been read. Only facts that get read on their own can hold guesses; for `dbus_signal` and `state` facts the initial value is real, and a signal value remembered from before a reboot is replaced by the initial value. A change of a confirmed input to an expr fact while another input is still a guess is not recorded in `since()` for the expr fact; it reports "never changed" until all inputs are confirmed, which errs on the side of not acting.

### Rules

A rule has a `while` condition (CEL) and two kinds of effect:

- **Outputs** are level-triggered: a state kept true for as long as the condition holds and undone when the condition stops holding. Examples: an inhibitor lock, a running unit, a power profile.
- **Actions** (`enter`, `exit`) are edge-triggered: run once when the rule starts or stops matching. Nothing is undone and nothing is re-checked. Examples: a notification, a command, a D-Bus call, setting a `state` fact.

The test for which one to use is whether there is anything to undo. A lock, a unit or a setting has an "off", so it is an output. A notification or a hibernate has none, so it is an action. Taking an inhibitor from an `enter` command would leak it: nothing releases it on exit or takes it again after a crash.

Rule keys are flat. `name`, `while`, `hold`, `priority`, `linger`, `enter` and `exit` are reserved; every other key must name a known output type, so typos fail at load.

### Outputs

Output types are compiled into the binary, because each needs more than an "apply" step. It must:

- read the current value,
- apply the desired value,
- restore what was there before, without overriding a manual change,
- merge claims from several rules.

A shell command cannot provide all four. A systemd unit can (`ExecStart` applies, `ExecStop` reverts, systemd supervises and logs), so `units` is the generic output and the set of native types stays small:

| Output | v1 |
|---|---|
| `units`: systemd units that should be running (user bus, and system bus where polkit allows it) | yes |
| `inhibit`: logind inhibitor locks (`sleep`, `idle`, `shutdown`, `handle-lid-switch`, ...) | yes |
| `power_profile` (power-profiles-daemon) | later |
| `gsettings` | later |
| own lid handling: hold the lid lock permanently and decide per rule what closing the lid does | later |

Actions in v1: `notify`, `command`, `dbus_call`, `set` (write a `state` fact). Later: delay inhibitors (run something before sleep).

Inhibitors are taken directly through `org.freedesktop.login1.Manager.Inhibit` and held as file descriptors, not as spawned `systemd-inhibit` processes. When the set of `what` values changes, the new lock is taken before the old one is closed, so there is no gap.

### Conflicts

When several matching rules claim the same output:

- Set-like outputs merge: `inhibit` is a union per `what`, `units` is a union.
- Scalar outputs (power profile, a setting) go to the rule with the highest priority. Rule order in the file is the default priority.

`status` shows for every output which rules wanted it and which one won.

### Releasing outputs

whilst only undoes what it did, and never fights a manual change:

- An inhibitor lock cannot drift: whilst holds its file descriptor, and closing it releases the lock.
- A unit is started when a rule starts wanting it, unless it is already running, in which case whilst does not own it and will not stop it. When no rule wants it any more, it is stopped only if whilst started it. A unit that stops while still wanted (a crash, or a manual stop) is left alone until no rule wants it and then one does again: restarting crashed services is systemd's job (`Restart=`). Ownership is persisted, so a restart of whilst does not forget which units it started. The same holds for system units, kept per bus: a name on the system bus is a different unit from the same name on the user bus.
- Scalar outputs (later) will restore the value from before they took it, unless it was changed in the meantime.

### Reconcile loop

Any fact change triggers an evaluation:

1. Evaluate `expr` facts, then rules, against the current facts.
2. Run `enter`/`exit` actions for rules whose match state changed (`set` actions apply at once and trigger another pass; the others are queued).
3. Compute the desired outputs from all matching rules.
4. Diff against what is currently held, and apply the difference.

The loop asks "what should be held right now", never "what transition just happened". A crash, a missed signal or a suspend is corrected on the next evaluation. At start and after resume (logind `PrepareForSleep(false)`), every source is re-read and the loop runs.

### Time

`since(name)` returns how long ago a fact last changed, measured in wall-clock time so that time spent suspended counts. The daemon has to wake when a time-based condition would flip, without polling. CEL exposes the checked AST, so at load time whilst finds comparisons of the form `since(x) <op> <duration literal>` and schedules a re-evaluation at `changed(x) + duration`. An expression that depends on time in any other way (`now`, or `hour`, `minute` and `weekday`, which are in local time) falls back to a coarse tick, 30s by default (`tick`), and `whilst check` warns about it.

### Persistence

State is kept in `~/.local/state/whilst/state.json`:

- Fact values and when they last changed, so `since()` survives a restart of whilst.
- Whether each rule matched, so a restart does not re-run enter actions.
- Output state that outlives the process (which units whilst started).
- Temporary rules not tied to a caller.

The file records the kernel's boot id. After a reboot, remembered values (except state facts) are treated as provisional guesses: whatever happened while the machine was off was not observed, so the first real read replaces them without counting as a change. Rule match state and output state (unit ownership) are dropped after a reboot, because the reboot undid what enter actions did and stopped the units; rules start fresh. Unnamed rules are named after a hash of their content, so reordering the file does not move state between rules.

### State machines

Most of what one would draw as a state machine falls out of facts and `since()`. "Protected for 30 minutes after unplugging" is just `!ac && since('ac') < duration('30m')`: the state is implied by the facts, so there is nothing to keep in sync.

Real state is needed when the past cannot be read from the current facts. Example: protect the lid for 30 minutes after unplugging, but end the protection early if the lid is opened in between. Once the lid is closed again, the facts no longer show it was opened.

A `state` fact covers this. Rules with `enter` actions are the transitions, and rules with outputs say what each state means:

```toml
[fact.carry]
state   = ["off", "protecting"]
initial = "off"

[[rule]]  # unplugged -> protecting
while = "!ac"
enter = [{ set = { carry = "protecting" } }]

[[rule]]  # lid opened or plugged in -> off
while = "carry == 'protecting' && (!lid || ac)"
enter = [{ set = { carry = "off" } }]

[[rule]]  # timeout -> off
while = "carry == 'protecting' && since('carry') > duration('30m')"
enter = [{ set = { carry = "off" } }]

[[rule]]  # what the state means
while   = "carry == 'protecting'"
inhibit = ["handle-lid-switch"]
```

State facts are persisted to disk so they survive a restart of whilst.

## Expression language

[CEL](https://cel.dev) via `cel.dev/cel-go`:

- Not Turing-complete, always terminates, no I/O, cost can be capped.
- Typed, with durations and timestamps built in.
- Expressions are parsed and type-checked at config load, so mistakes surface at start, not when the lid closes.
- Widely used for policy (Kubernetes admission policies, Envoy, Google Cloud IAM conditions).

`expr-lang/expr` was the alternative. CEL wins on safety and types.

## Config

TOML, at `~/.config/whilst/config.toml`.

```toml
include = ["preset:upower"]   # defines ac, battery

[fact.lid]
dbus = { bus = "system", service = "org.freedesktop.login1", path = "/org/freedesktop/login1", interface = "org.freedesktop.login1.Manager", property = "LidClosed" }

[fact.ssid]
command = "nmcli -t -f ACTIVE,SSID dev wifi list --rescan no | sed -n 's/^yes://p'"
on_dbus = ["type='signal',sender='org.freedesktop.NetworkManager',interface='org.freedesktop.NetworkManager.Connection.Active',member='StateChanged'"]

[fact.home]
expr = "ssid == 'MyHomeWifi'"

[[rule]]
name    = "home-on-mains"
while   = "ac && home"
inhibit = ["sleep", "handle-lid-switch"]

[[rule]]
name    = "just-unplugged"
while   = "!ac && since('ac') < duration('30m')"
inhibit = ["handle-lid-switch"]
enter   = [{ notify = "Lid will not suspend for 30 minutes" }]

[[rule]]
name    = "mains-elsewhere"
while   = "ac"
inhibit = ["sleep"]

[[rule]]
name  = "sync-at-home"
while = "home && ac"
units = ["syncthing.service"]
```

## CLI

```
whilst run [--no-watch]        run the engine (what the user service starts); reloads the
                               config when it changes unless --no-watch
whilst status                  facts, matching rules, held outputs and who owns them
whilst why <target>            why an output or rule is in its current state
whilst check [config]          parse and type-check the config, warn about tick fallbacks
whilst ensure [dur] <outputs>  temporary top-priority rule; also --until, --while,
                               --pid and `-- command` (replaces block-sleep's modes)
whilst release [id | --all]    drop temporary rules
whilst reload                  re-read the config
whilst presets [name]          list or print presets
```

The running engine owns `io.github.PeterSR.Whilst` on the session bus, which the CLI uses. A temporary rule tied to a command or pid is attached to the CLI's bus connection and dropped when that connection goes away.

## Platform

### Dependencies

Hard:

- Linux.
- systemd: logind for inhibitors, lid state and sleep, and the per-user manager (`systemd --user`) to run whilst as a user service and to manage units.
- D-Bus, both the system and the session bus.
- polkit, which lets an active session take blocking inhibitors and manage user units without root, and decides which system units a user may start and stop.

Optional, only when a config uses them: UPower, NetworkManager, power-profiles-daemon, a desktop notification daemon, GNOME (gsettings).

Build-time: Go, `cel-go`, `godbus`.

Distributions using elogind instead of systemd provide the same logind API, so inhibitors could work there, but unit outputs would not.

### Moving parts

- **systemd-logind** owns the lid and power keys, inhibitor locks, `LidClosed`/`Docked`, `PrepareForSleep`, and the `Suspend`/`Hibernate` calls. Configured by `logind.conf` (`HandleLidSwitch*`, `LidSwitchIgnoreInhibited`).
- **systemd (PID 1)** carries out sleep (`sleep.target`, `systemd-suspend.service`) and runs units.
- **D-Bus** (dbus-daemon or dbus-broker) is the transport for everything.
- **polkit** authorises the session.
- **UPower** for power, **NetworkManager** for networks, **power-profiles-daemon** for profiles.
- **The desktop**: in GNOME, gsd-power performs idle suspend by calling logind and takes its own `handle-lid-switch` lock when an external monitor is attached. gnome-session keeps a separate inhibit list for "idle".

With the default `LidSwitchIgnoreInhibited=yes`, a `sleep` inhibitor does not stop lid suspend. Only a `handle-lid-switch` inhibitor does. If that lock is released while the lid is closed, logind is expected to suspend shortly after.

### Process and privilege

- Runs as a systemd user service in the graphical session.
- System units need no helper: whilst calls `StartUnit` and `StopUnit` on the system manager like on the user manager, and polkit decides per unit and per user (a rule for `org.freedesktop.systemd1.manage-units` that allows one unit). A denial is shown in status as an error on that unit and retried like any output that could not be applied. Root-only settings that polkit cannot grant would come through a small privileged helper with a narrow interface, never by running the engine as root.

## v1 scope

Enough to replace systemd-block-sleep completely and prove the model:

- Fact sources: `dbus`, `dbus_signal`, `unit`, `command`, `file`, `expr`, `state`, clock.
- Rules in CEL, with `since()` and timer scheduling.
- Outputs: `inhibit`, `units`.
- Actions: `notify`, `command`, `dbus_call`, `set`.
- CLI: `run`, `status`, `why`, `check`, `ensure`, `release`, `reload`, `presets`.
- Presets for UPower, logind and NetworkManager.

## Architecture

The engine and the platform are separate modules, so other front ends can reuse the engine:

- **The engine** ([github.com/PeterSR/whilst](https://github.com/PeterSR/whilst), its own repository) holds everything platform-independent: values, CEL, config loading, the reconcile loop, temporary rules, status and the state file. It implements `expr` and `state` facts and the `set` action. Everything else comes in through a registry and a few interfaces:
  - A *fact kind* names the key that selects it in a fact table, the other keys it accepts, and whether it is *rereadable* (gets a real reading on its own, so its initial or remembered value is only a guess, and the engine waits for it at start). It parses its table into a spec with a stable *source key*, which tells whether a reload or an edit changed where a value comes from and is stored in the state file.
  - An *action kind* parses its table; the application's runner carries the action out.
  - *Outputs* parse rule values, reconcile the wanted state and persist what must survive a restart, as before.
  - *Sources* deliver values for a fact, already in the core's normal form.
  - *Lifetimes* tell the engine when the caller of a temporary rule has gone.
  - Presets are an `fs.FS` the application supplies; the boot id is a string it supplies.
- **The application** (`systemd/`, `cmd/whilst`) provides all of the above for a Linux desktop: the D-Bus hub and match rules, the D-Bus, unit, command and file sources and their triggers (`on_dbus`, `on_dbus_session`, `every`), conversion of D-Bus values, the `inhibit` and `units` outputs, the `notify`, `command` and `dbus_call` actions, the D-Bus API, the config watcher and the presets.

Unnamed rules are named after a hash of their content. An action of a newly registered kind adds only itself to that hash, so adding a kind never renames rules that do not use it. The three original kinds (notify, command, dbus_call) declare a legacy hash slot, which keeps the names earlier versions gave. Source keys of the built-in kinds keep the layout earlier versions stored, so existing state files load unchanged.

## Open questions

- Whether block-sleep's `hold <pipe>` mode is worth keeping. `ensure` with `--pid`, a command, or `release` covers the cases seen so far.
- Settled: system units go through the system bus with polkit deciding, and need no privileged helper. Whether other root-only settings need one is still open.
- Whether to publish facts to MQTT so Home Assistant can see them.
