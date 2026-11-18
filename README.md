# whilst

Keep your Linux machine in the state your rules ask for, for as long as their conditions hold.

```toml
include = ["preset:upower", "preset:networkmanager"]

[fact.home]
expr = "ssid == 'MyHomeWifi'"

[[rule]]
name    = "home-on-mains"
while   = "ac && home"
inhibit = ["sleep", "handle-lid-switch"]
units   = ["syncthing.service"]

[[rule]]
name    = "just-unplugged"
while   = "!ac && since('ac') < duration('30m')"
inhibit = ["handle-lid-switch"]
enter   = [{ notify = "Lid will not suspend for 30 minutes" }]
```

`whilst` watches facts about the machine (power, network, lid, systemd units, the clock, and anything else on D-Bus or behind a command) and evaluates rules written in [CEL](https://cel.dev). While a rule matches, its outputs are kept true: an inhibitor lock is held, a unit is running. When the rule stops matching, the outputs are released and whatever whilst changed is undone. Rules can also run actions once, when they start or stop matching.

Think Tasker or ControlPlane for a Linux desktop, built on systemd and D-Bus. It replaces [systemd-block-sleep](https://github.com/PeterSR/systemd-block-sleep).

See [DESIGN.md](DESIGN.md) for the model and the reasoning behind it.

## Install

Requires Linux with systemd (logind and a user manager) and D-Bus. Download `whilst-linux-amd64` from the [latest release](https://github.com/PeterSR/systemd-whilst/releases/latest), then:

```sh
chmod +x whilst-linux-amd64
./whilst-linux-amd64 install
```

`whilst install` copies the binary to `~/.local/bin/whilst`, installs the systemd user service and starts it. If the binary already sits in a directory on your `PATH` (for example after `go install`), it is used where it is. To upgrade, run `install` again from the new binary.

`whilst doctor` checks the setup (buses, the service, logind and its permissions, the config) and says what to fix.

From source (Go 1.25+):

```sh
make install           # builds, then runs ./whilst install
# or
go install github.com/PeterSR/systemd-whilst/cmd/whilst@latest && whilst install
```

Then write `~/.config/whilst/config.toml` (start from [examples/config.toml](examples/config.toml)) and check it:

```sh
whilst check
whilst status
```

The service reloads the config by itself when the file or any file it includes changes, half a second after the last write. A config that does not load is not applied: whilst keeps the running one and shows the error in `whilst status` and `whilst doctor`. Deleting the config file is not applied either; to run with no rules, empty the file or run `whilst reload` after deleting it. `whilst reload` reloads at once and prints the result (`systemctl --user reload whilst` also works), and `whilst run --no-watch` turns automatic reloading off.

Without a config file whilst runs with no rules, and `whilst ensure` still works.

## Concepts

**Facts** are named values that whilst keeps current, each with the time it last changed. **Rules** have a `while` condition over facts. While it is true, the rule's **outputs** are held. When it becomes true or false, the rule's `enter` or `exit` **actions** run once.

The difference between outputs and actions is whether there is anything to undo. A lock or a running unit has an "off", so it is an output and whilst releases it for you. A notification has none, so it is an action.

whilst works by reconciling, not by reacting to transitions: after every change it works out what should be held right now and makes it so. A missed signal, a crash or a suspend is corrected on the next evaluation.

## Facts

Every fact is declared in config with exactly one source.

| Source | Value | Re-read when |
|---|---|---|
| `dbus = { service, path, interface, property, bus }` | the property | it changes (PropertiesChanged), the service restarts, triggers |
| `dbus_signal = { member, interface, path, sender, bus, arg }` or `{ match = "..." }` | the signal body, or argument `arg` | the signal arrives |
| `unit = "name.service"` or `{ name, bus }` | the unit's ActiveState, like `"active"` | it changes, triggers |
| `command = "shell command"` | trimmed stdout (`output = "stdout"`), exit status as a bool (`"status"`), or parsed JSON (`"json"`) | triggers |
| `file = "/path"` | trimmed contents | polled, every 5s by default |
| `expr = "CEL"` | the expression over other facts | any input changes |
| `state = ["a", "b"]` or `state = true` (then `initial` is required) | set only by `set` actions | a rule sets it |

`bus` is `system` (default for D-Bus sources) or `session`/`user` (default for units).

Triggers for sources that can be re-read:

- `on_dbus = ["match rule", ...]` and `on_dbus_session = [...]`: re-read when a matching signal arrives.
- `every = "30s"`: re-read on an interval.

Other fact keys:

- `type`: `bool`, `int`, `uint`, `double`, `string`, `list`, `map` or `dyn` (default). Values are converted where it is obvious (`"1"` to `true`), and CEL type-checks rules against it.
- `transform`: a CEL expression applied to the raw value, which it sees as `value`. For example `transform = "!value"`.
- `initial`: the value before the first read. For facts that are read (D-Bus properties, units, commands, files) it is only a guess, and rules reading the fact wait for a real reading. For `dbus_signal` and `state` facts, which are never read, it is taken as the real value; a state fact defaults to its first allowed value.
- Durations in config always need a unit: `"30s"`, `"5m"`, `"1h"`.
- `debounce = "5s"`: a change must hold this long before it counts. Blips shorter than that are ignored. Works on `expr` facts too, which is often the best place for it (`home` rather than `ssid`).

If a read fails, the fact keeps its last good value and `status` shows the error.

### Presets

Common facts ship inside the binary as ordinary config: `include = ["preset:upower"]`. Run `whilst presets` to list them and `whilst presets upower` to read one.

| Preset | Facts |
|---|---|
| `upower` | `ac` (on mains), `battery` (percent) |
| `logind` | `lid` (closed), `docked` |
| `networkmanager` | `connection` (primary connection name), `ssid`, `online`, `metered` |

Other files can be included by path, relative to the including file.

## Rules

```toml
[[rule]]
name     = "unique-name"        # optional; used in status, logs and state
while    = "CEL condition"      # required
hold     = "CEL condition"      # optional; once matching, keep matching while this is true
priority = 10                   # optional; higher first
linger   = "1m"                 # optional; keep matching this long after the condition ends
inhibit  = [...]                # outputs: any number
units    = [...]
enter    = [{ ... }, ...]       # actions: optional
exit     = [{ ... }, ...]
```

Any other key is an error, so a misspelt output is caught at load.

### Conditions

Conditions are [CEL](https://github.com/google/cel-spec/blob/master/doc/langdef.md). Facts are variables. Also available:

- `since('fact')`: how long ago the fact last changed, as a duration. A fact never seen to change is treated as having changed long ago.
- `now` (timestamp), and `hour`, `minute`, `weekday` (1 is Monday) in local time.

A comparison like `since('ac') < duration('30m')` costs nothing: whilst works out when it will flip and wakes up exactly then. Conditions that use the clock in other ways (`hour >= 21`) are re-checked every 30 seconds, and `whilst check` says so.

Two settings can go at the top of the main config: `tick = "30s"` (how often clock-dependent conditions are re-checked) and `settle = "3s"` (how long whilst waits at start for every fact to be read before the first evaluation).

If a condition cannot be evaluated (for example a fact has no value yet), the rule keeps its previous state and `status` shows the error. The same goes for a condition that reads a fact whose value is still a guess (an `initial` value, or one remembered from before a reboot): whilst does not act on it until the fact has really been read.

### Hold

`hold` gives a rule hysteresis. The rule starts only when `while` is true, but once it matches it stays on for as long as `while` or `hold` is true. This keeps a rule from flapping around a threshold:

```toml
[[rule]]
name    = "save-power"
while   = "battery < 20"     # start below 20%
hold    = "battery < 30"     # and keep going until 30%
inhibit = ["idle"]
```

`status` marks a rule that matches only through its hold, and `why` shows both conditions. `since()` thresholds in `hold` are scheduled exactly, like those in `while`. Adding or changing `hold` on an unnamed rule gives it a new name, as editing `while` does. A named rule keeps its state across edits, so if it matches when you reload, a new `hold` can keep it on even though the new `while` has not been true yet.

### Linger

`linger = "1m"` keeps a rule matching for a minute after its condition (`while`, or `hold` if it has one) turns false: its outputs stay held and its exit actions wait. If the condition turns true again within that minute, the rule simply carries on, with no exit or enter actions. Use it to bridge the moment when one rule hands over to another and the new state is not fully visible yet. For example, plugging a closed laptop into a monitor that also powers it brings power at once but the monitor a few seconds later. Without a linger on the lid rule, the lid lock is gone in between and logind suspends. The linger also applies when a condition ends by time (`since(...)`), and survives a restart of whilst.

Give rules that have actions a `name` (names starting with `ensure:` are reserved). Rule state is remembered by name; an unnamed rule is named after its content, so editing it (other than `priority` or `linger`) makes it a new rule (its exit actions run, then the new rule's enter actions). When a reload removes a rule that matched, its exit actions run. A rule removed while whilst was not running gets no exit actions, because whilst no longer knows them.

### Outputs

| Output | Effect while the rule matches |
|---|---|
| `inhibit = ["sleep", ...]` | Holds a blocking logind inhibitor lock. Types: `sleep`, `idle`, `shutdown`, `handle-lid-switch`, `handle-power-key`, `handle-suspend-key`, `handle-hibernate-key`, `handle-reboot-key`. |
| `units = ["name.service", ...]` | Keeps user units running. |

When several rules want the same output, their wishes are merged: one lock covers the union of all wanted types, and a unit runs while any rule wants it. When the merged set of lock types changes, the new lock is taken before the old one is released.

In `whilst status`, a unit shown as held is one whilst has claimed for a rule; it may since have stopped. Units are started when a rule starts wanting them and stopped when no rule wants them any more, but only if whilst started them. A unit that was already running is left running. A unit that stops while wanted (it crashed, or you stopped it) is not restarted until no rule wants it and then one does again: crash recovery is systemd's job (`Restart=`), and overriding a manual stop would be rude.

Note on lids: with logind's default `LidSwitchIgnoreInhibited=yes`, a `sleep` lock does not stop the lid from suspending. Only `handle-lid-switch` does.

### Actions

```toml
enter = [
  { notify = "Short text" },
  { notify = { summary = "Title", body = "Details", urgency = "low" } },
  { command = "shell command", timeout = "1m" },   # gets WHILST_RULE and WHILST_EVENT; output goes to the journal
  { dbus_call = { service = "org.freedesktop.login1", path = "/org/freedesktop/login1",
                  method = "org.freedesktop.login1.Manager.Hibernate", args = [false], signature = "b" } },
  { set = { mode = "away" } },                     # writes state facts
]
```

Actions run one at a time, in the order listed, and one rule's exit actions finish before the next rule's enter actions start. A slow command therefore delays the actions after it. The exception is `set`, which is applied immediately, before the rule's other actions run, so that the next evaluation already sees the new state.

A `command` may start a background job (`some-app &`): whilst returns when the shell exits and leaves the job running. The job still belongs to the whilst service, though, and stops when the service stops or restarts. To start something that should outlive it, use `systemd-run --user some-app`.

### State machines

Most state follows from facts and `since()`. When the past cannot be read from the current facts, use a `state` fact. Rules with `set` actions are the transitions, and rules with outputs say what each state means:

```toml
[fact.carry]
state = ["off", "protecting"]

[[rule]]  # unplugged: protect the lid
while = "!ac"
enter = [{ set = { carry = "protecting" } }]

[[rule]]  # lid opened or plugged in again: stop
while = "carry == 'protecting' && (!lid || ac)"
enter = [{ set = { carry = "off" } }]

[[rule]]  # or after 30 minutes
while = "carry == 'protecting' && since('carry') > duration('30m')"
enter = [{ set = { carry = "off" } }]

[[rule]]
while   = "carry == 'protecting'"
inhibit = ["handle-lid-switch"]
```

## Command line

```
whilst run [--config PATH] [--debug] [--no-watch]
                                       run the engine (the user service does this)
whilst status [--json]                 facts, rules and outputs right now
whilst why TARGET                      why an output is or is not held, or why a rule
                                       does or does not match
whilst check [PATH]                    load and type-check a config
whilst ensure [DURATION] OUTPUT=VALUE... [--while CEL] [--until TIME] [--name NAME]
              [--pid PID] [-- COMMAND...]
whilst release ID | NAME | --all       drop temporary rules
whilst reload                          re-read the config
whilst presets [NAME]                  list or print presets
whilst install                         install the binary and user service, or upgrade
whilst uninstall                       stop and remove the user service
whilst doctor                          check the setup and say what to fix
whilst version
```

`whilst ensure` adds a temporary rule that wins over every config rule:

```sh
whilst ensure 2h inhibit=sleep                     # for two hours
whilst ensure --until 22:00 inhibit=sleep,idle     # until a time
whilst ensure inhibit=sleep -- make -j8            # while a command runs (exit code is passed on)
whilst ensure inhibit=sleep --pid 4242             # while a process lives
whilst ensure --while '!ac' inhibit=idle           # while a condition holds, until released
```

`ensure` applies the rule before it returns, and with a command it refuses to start the command if a lock could not be taken. A condition that cannot be checked yet (a fact not read since start) is not a refusal: the rule applies as soon as it can. Rules tied to a command or pid end when that `whilst ensure` process exits; if the whilst service restarts meanwhile, the rule is added again automatically. SIGTERM and SIGHUP sent to `whilst ensure` are passed on to the command. Other temporary rules survive restarts of whilst, unless their `--while` condition no longer compiles against the config (a reload keeps such a rule in its current state instead). Signals that were ignored when `whilst ensure` started (for example under `nohup`) stay ignored, for it and for the command.

## Uninstall

```sh
whilst uninstall       # stops and removes the user service
rm ~/.local/bin/whilst # or wherever the binary is
```

Config and state stay behind in `~/.config/whilst` and `~/.local/state/whilst`. Remove them too for a full cleanup.

## Files

| Path | What |
|---|---|
| `~/.config/whilst/config.toml` | config (`$WHILST_CONFIG` or `--config` override) |
| `~/.local/state/whilst/state.json` | fact values and change times, rule state, units whilst started, temporary rules |

State lets `since()` and rule state survive a restart of whilst, so enter actions do not re-run. After a reboot, remembered values are treated as unconfirmed guesses: what changed while the machine was off was not observed, so it does not count as a change. Rule state and unit ownership are dropped after a reboot (the reboot undid what enter actions did and stopped the units), so rules start fresh and enter actions run again. State facts and temporary rules are kept across reboots.

## D-Bus API

The engine owns `io.github.PeterSR.Whilst` on the session bus, object `/io/github/PeterSR/Whilst`, interface `io.github.PeterSR.Whilst1`, with methods `Status() -> s` (JSON), `Ensure(s spec) -> s` (JSON `{"id": ..., "problems": [...]}`; the rule is applied before the reply), `Release(s id) -> i`, `Reload() -> s`.

## License

MIT
