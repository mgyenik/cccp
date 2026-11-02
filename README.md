# cccp — Claude Code control panel

One place that shows every Claude Code and Antigravity CLI (`agy`) session
running in tmux, and which of them need you:

```
cccp  ⚠ 1 needs you  ⏎ 2 your turn  ● 3 working  ○ 6 idle  ❄ 14 cold            daemon
NEEDS YOU
 ⚠  CC web:2     Billing API refactor        Allow Bash: rm -rf build        2m  warm 58m  290k
YOUR TURN
 ⏎  CC fw:3      Bootloader bring-up         Want me to flash the image?     4m  warm 56m  501k
WORKING
 ●  CC audio:1   Gain staging                Bash: cargo test --release     12m            454k
IDLE
 ○~ AG lab:0     Sensor dashboard            › document the architecture     3d            66k
COLD
 ❄  CC docs:4    Release notes draft         › commit this and push          1h  cold 31m  496k
```

* **NEEDS YOU**: blocked on a permission prompt, a question, or an API error.
* **YOUR TURN**: finished a turn you haven't looked at yet. Looking at the
  pane (on a focused terminal) clears it.
* **WORKING / IDLE / COLD**: COLD means the prompt cache has expired, so the
  next message re-sends the whole context uncached. IDLE is sorted by soonest
  cache expiry.
* `~` marks a state that is **inferred** (a session without hooks) rather
  than observed.

See [docs/architecture.md](docs/architecture.md) for how it works, and
[docs/agy-hooks.md](docs/agy-hooks.md) for the agy hook research.

## Install

```sh
go build -o cccp ./cmd/cccp     # Go ≥ 1.22; the toolchain fetches the version go.mod needs
./cccp install                  # asks before each step
cccp doctor
```

`cccp install` (each step is confirmed; `--yes` skips the questions, `--only
bin,claude,agy,systemd,tmux` picks steps):

1. **bin**: copies the binary to `~/.local/bin/cccp`.
2. **claude**: installs the `cccp` Claude Code plugin from a local
   marketplace in `~/.local/share/cccp/marketplace`. Its hooks only observe:
   they always exit 0 and print nothing, and if the binary is missing they
   exit silently. Takes effect in sessions started (or `/reload-plugins`'d)
   afterwards.
3. **agy**: installs observe-only hooks to `~/.gemini/config/plugins/cccp`
   (`SessionStart`, `PreInvocation`, `PostToolUse`, `Stop`; deliberately not
   `PreToolUse`, see docs/agy-hooks.md).
4. **systemd**: installs and starts `cccp.service` (user unit).
5. **tmux**: adds one `source-file` line to `~/.tmux.conf`. The snippet sets
   `focus-events on`, event hooks (index `[97]`), the key bindings, and the
   status-right / window-status formats. The previous values of those options
   and keys are saved, and `cccp uninstall` restores them.

Sessions started before the plugin was installed are still listed: Claude
Code ≥ ~2.1.28x through its session registry, older ones and pre-plugin agy
sessions through tmux/`/proc` discovery with inferred state. Restart them for
full tracking.

## Use

| Where | Key / command | Does |
|---|---|---|
| tmux | `prefix a` | jump to the most urgent session that needs you; repeat to cycle |
| tmux | `prefix A` | open the cockpit popup |
| cockpit | `enter` | jump to the session (resumes it in a new window if it ended) |
| cockpit | `tab` / `shift+tab` | next / previous session needing you |
| cockpit | `/` | filter by title, project, location, state, prompt |
| cockpit | `?` | full-text search over all transcripts; `enter` opens or resumes |
| cockpit | `a` | set an alias for the session |
| cockpit | `m` | mark seen |
| cockpit | `p` | preview: live pane → conversation → off |
| cockpit | `e` | show ended sessions |
| cockpit | `r` | rescan everything |
| shell | `cccp ls [--all] [--json]` | list sessions |
| shell | `cccp search TERMS…` | search transcripts (`"quoted phrase"` supported) |
| shell | `cccp status` | one-line counts |
| shell | `cccp jump --next` / `cccp jump KEY` | jump from scripts |

The status line shows the counts (`⚠1 ⏎2 ●3 ○6 ❄14`), and each window name is
prefixed with the state of the most urgent session in it.

## Configure

`~/.config/cccp/config.toml` (all optional; `systemctl --user kill -s HUP cccp`
reloads it):

```toml
notify = true                  # desktop notification when a session newly needs you (pane not visible)
notify_command = ["notify-send", "--app-name=cccp", "--expire-time=10000"]
summaries = "ac"               # one-line "what it needs from you" via claude -p: off | ac | always
summary_model = "haiku"
default_cache_ttl = "1h"       # used when a transcript doesn't reveal the cache bucket
ended_retention = "168h"       # how long ended sessions are kept
quiet_after = "10s"            # unmanaged session: pending tool + quiet pane → probably waiting for approval
```

Summaries run `claude -p` with no tools, no settings and no session
persistence, and use your Claude subscription. With `"ac"` they only run on
mains power.

## Files

| Path | Content |
|---|---|
| `~/.local/state/cccp/sessions/` | hook-observed session state (written by hooks) |
| `~/.local/state/cccp/daemon.json` | seen marks, aliases, summaries |
| `~/.local/state/cccp/hook-errors.log` | hook problems (never shown to the agents) |
| `~/.cache/cccp/text/` | extracted transcript text for search (rebuildable) |
| `$XDG_RUNTIME_DIR/cccp.sock` | daemon socket |

## Uninstall

`cccp uninstall` reverses every install step (confirmed one by one). State
and cache are left in place.
