# cccp — Claude Code control panel: architecture

Status: v1 design, written 2026-11-02 alongside the first implementation.

## 1. Problem

Dozens of long-lived coding-agent sessions (Claude Code and Antigravity CLI,
`agy`) live in tmux windows across several tmux sessions. There is no single
place that answers:

* which sessions are blocked on me right now (permission prompt, question)?
* which finished a turn I haven't looked at yet?
* which are working, which are idle, and which have let their prompt cache
  expire (so the next message re-sends the whole context uncached)?
* what is each one about, and where was that conversation where we did X?

## 2. Goals and non-goals

Goals

* Correct, timely state for every agent session in tmux, with the provenance
  of each fact visible (observed from a hook vs. inferred from a heuristic).
* Zero-friction navigation: jump to the next session that needs me, fuzzy
  find by title/project, full-text search across transcripts, resume dead
  sessions.
* Battery friendly: event driven, no polling loops, near-zero wakeups at idle.
* Degrades instead of failing: with the daemon down, `cccp ls` and the
  cockpit still show correct state from files.

Non-goals (v1)

* Owning PTYs. tmux already owns the terminals, and does it better than a
  re-implementation would; sessions must survive cccp crashing.
* Approving permission prompts from the cockpit. The cockpit shows the pending
  tool call; you approve in the session itself, with full context in front of
  you. (Possible later as an explicit action.)
* The Antigravity desktop/IDE app (not terminal based). Only `agy` CLI.

## 3. Ground truth: what each source can and cannot tell us

All of this was verified on the target machine (Claude Code 2.1.283–2.1.316,
agy CLI, tmux 3.4) before writing code.

| Source | Gives | Does not give | Cost |
|---|---|---|---|
| **Claude Code hooks** (documented, stable) | Exact lifecycle events: prompt submitted, each tool start/end, permission request (immediately), question tool (`AskUserQuestion`), elicitation, turn end (`Stop`, with `last_assistant_message`), API error (`StopFailure`), compaction, session end. `$TMUX_PANE` via inherited env. | Anything for sessions started before the plugin was installed. No event when the user *interrupts* (Stop does not fire) or *manually denies* a permission. | One short process per event. |
| **Claude session registry** `~/.claude/sessions/<pid>.json` | Live `status` (`busy`/`waiting`/`idle`), `waitingFor` (`permission prompt`, `input needed`, …), `statusUpdatedAt`, pid + `procStart`, tmux pane (`"tmux":"sess:@win.%pane"`), session name, `kind` (interactive/background). Covers interrupts and manual denials. | Only Claude ≥ ~2.1.28x; older long-running processes never registered. Not a documented-stable file format (the documented interface is `claude agents --json`, which spawns a node process — too heavy to poll). | inotify; files rewritten only on status change. |
| **Claude transcript** `~/.claude/projects/*/<sid>.jsonl` | `ai-title` (generated session title), `last-prompt`, every assistant message with timestamp and `usage` incl. `cache_creation.ephemeral_{5m,1h}_input_tokens` (⇒ exact cache TTL bucket and context size). | Live state (it lags, and is written asynchronously). Can be large (80 MB). | Tail read on demand; incremental. |
| **tmux** | Panes, which pane each client is looking at, focus, `pane_title` (Claude sets it to `✳ <ai title>` — even old versions), pane output activity time, hooks for focus/title/exit events. | Agent state. | One `tmux` exec per query; hooks are event driven. |
| **/proc** | `TMUX_PANE` from a process's environ, process tree (pane shell → agent), process start time (pid reuse guard), pidfd for exit notification. | — | Cheap; pidfd exit notification is event driven. |
| **agy hooks** (`hooks.json` in a plugin) | `SessionStart` (lazily, on first prompt), `PreInvocation` (model about to run), `PostToolUse`, `Stop` (with `terminationReason`: `NO_TOOL_CALL`, `USER_CANCELED`, `ERROR`, …). camelCase payload with `conversationId`, `transcriptPath`. | No permission-request or notification event. `PreToolUse` exists but is not used (it carries permission-decision semantics; see `docs/agy-hooks.md`). | Synchronous only. |
| **agy summaries db** `~/.gemini/antigravity-cli/conversation_summaries.db` | Conversation title, workspace, last modified. | Live state. | sqlite read, on demand. |

Consequences for the design:

1. **Hooks are the primary event source**, because they are documented and
   carry the richest detail (which tool, what question, the final message).
2. **The registry is the correction source** for Claude: it is the only thing
   that sees interrupts and manual denials, and it covers sessions that started
   before the plugin. Merge rule: *latest observation wins* (section 5).
3. **Transcripts supply the slow-moving facts**: title, last prompt, cache
   TTL and last-activity time, context size.
4. **Sessions with neither hooks nor registry** ("unmanaged": old Claude
   versions, agy started before the plugin) are discovered from tmux/proc and
   classified by heuristics. Their state is always shown as *inferred*
   (`~` marker), never presented as observed.

## 4. Components

One Go binary, `cccp`, with subcommands; plus three integration bundles.

```
            Claude Code ──hooks──┐            agy ──hooks──┐
                                 ▼                         ▼
                       cccp hook claude            cccp hook agy
                                 │  (flock, reduce, atomic rename)
                                 ▼
              ~/.local/state/cccp/sessions/<agent>-<id>.json   ◄── source of truth for hook-observed state
                                 │ inotify
   ~/.claude/sessions/*.json ────┤ inotify
   transcripts (on demand) ──────┤
   tmux (exec + hook pokes) ─────┤ unix socket  ◄── cccp notify tmux …  (tmux set-hook)
   /proc + pidfd ────────────────┤
                                 ▼
                              cccpd  (cccp daemon; systemd --user)
                                 │ gather → core.Compute → machine.Step → effects
        ┌────────────────────────┼───────────────────────────┬───────────────┐
        ▼                        ▼                           ▼               ▼
  tmux options             unix socket subscribers     notify-send      summarizer
  @cccp_status (global)    (cockpit TUI, cccp ls/jump)  (unseen          (optional,
  @cccp_glyph (window)                                   attention)       AC power)
```

### 4.1 `cccp hook <claude|agy>` — event capture

* Reads the hook JSON from stdin, timestamps it at process start
  (`time.Now()` is the first thing main does), and applies it to the session's
  state file under an exclusive `flock`, then writes via temp file + rename.
* The reducer is a pure function `(State, Event) → State`. Out-of-order
  events (Claude runs `Notification` asynchronously) are handled by
  timestamp: an event older than the state's last activity change updates
  metadata only, never the activity.
* Records: pid of the agent process (found by walking parent pids to the
  first `claude`/`agy` ancestor, plus its start time to guard against pid
  reuse), `$TMUX_PANE`, cwd, transcript path, permission mode, current tool
  and a truncated input preview, pending question text, last prompt
  (truncated), last assistant message (truncated), error, counters.
* Must never interfere with the agent: always exits 0, prints nothing for
  Claude (agy requires JSON on stdout for some events; see 4.6), never blocks
  on the daemon, and runs in a few milliseconds. `CCCP_INTERNAL=1` in the
  environment makes it a no-op (used by cccp's own `claude -p` summarizer).

### 4.2 `cccp daemon`: event loop and effects

The daemon is an imperative shell around a pure state machine (section 10):
one goroutine turns OS events into machine inputs and executes the effects
the machine returns. Event sources:

* fsnotify on the cccp state dir and on `~/.claude/sessions/`
* pidfd exit notifications (one epoll goroutine for all tracked pids)
* unix-socket messages: tmux pokes (focus and layout events), UI requests
  (subscribe, refresh, seen, alias, reload)
* one timer, armed for the earliest deadline the machine names (a prompt
  cache expiring, or a pending memory save)
* SIGHUP: reload the configuration

Each event marks which sources are stale (`daemon.Scope`): a hook file change
rereads that file; a session appearing, vanishing or changing process
triggers a tmux reread and process rescan; a focus change rereads tmux only.
Bursts are drained into one observation. Effects are applied only for
actual changes:

* **tmux decoration**: global user option `@cccp_status` (counts, styled),
  per-window `@cccp_glyph`; written only when the value changes, and re-sent
  in full after a tmux restart or a failed write.
* **subscribers**: the snapshot is pushed to connected cockpits.
* **notifications**: when a session newly needs you and its pane is not
  visible on a client that is looking (4.7, focus).
* **summaries** (optional): see 4.8.

Idle cost: all goroutines block in the kernel (inotify read, epoll_wait,
accept, one timer). See section 8.

### 4.3 Session identification (`core.identify`)

Session identity is `(agent, session id)`; for unmanaged sessions whose id
can't be determined, `(agent, "pane:%N")`.

Sources, in order:

1. Hook state files (hook-observed sessions). Print-mode runs are skipped.
2. Claude registry entries of live user sessions (liveness: pid and
   `procStart`; `sdk*` entrypoints, i.e. `claude -p`, are skipped).
3. One current session per process: the registry's, else the hook session
   with the latest event; others on that process are superseded (a `/clear`
   without `SessionEnd`).
4. Agent processes under tmux panes (the shallowest non-print-mode `claude`
   or `agy` in each pane's process tree). A process no session claims is
   **unmanaged**. Its Claude session id comes from `--resume <uuid>` if
   present, otherwise from matching the pane title (`✳ <title>`) against
   `ai-title` entries in its project directory (newest untaken match). An
   agy process gets the newest untaken conversation in its directory modified
   since it started. Processes are matched newest first, and each
   conversation is assigned once.

Pane association, in order: the process tree (an agent below a pane's
shell: always this server's), the registry's `tmux` field, the hook's
`$TMUX_PANE`. Pane ids (`%3`) are unique only within one tmux server, and
neither the registry nor `$TMUX_PANE` names the server, so each claim
carries its server's socket: the hook records it from `$TMUX`, and for a
registry entry it is read once from the process's own `$TMUX` in
`/proc/<pid>/environ` (live processes only: a dead one's pid may be
reused). A claim from another server is ignored (the session is alive but
in no pane of ours), so a second server (`tmux -L work`) cannot lend its
`%3` to an unrelated pane here. Hook state written before servers were
recorded has no socket and is trusted until its next event.

### 4.4 State model

```
Activity:   working | needs_approval | needs_input | idle | error | ended | unknown
Provenance: observed (hook or registry)  |  inferred (heuristic)
```

Derived per session:

* **Attention**: `needs_approval` and `needs_input` always demand attention.
  A **completion** (turn ended: Stop, StopFailure, agy Stop, or a registry
  busy→idle transition) creates an attention item stamped with its time. The
    item is cleared when the session's pane is *seen* — visible on a tmux
  client that is looking (4.7, focus) — at or after that time, or when a new prompt is submitted. A
  completion that happens while you are looking at the pane is seen
  immediately.
* **Cache**: last main-thread model activity time (transcript) + TTL, where
  the TTL comes from the last request's `cache_creation` split (1h if it wrote
  1h tokens, 5m if it wrote 5m tokens; else the previous request's bucket;
  else the configured default). `warm until T` / `cold since T`.
* **agy's cache** is estimated (`Cache.Fades`). agy runs Gemini 4 models,
  which have only implicit caching: best effort, with no documented
  lifetime; explicit caching, which has a TTL, "is not supported … for
  Gemini 4 Argon and onward" ([Google's caching
  docs](https://ai.google.dev/gemini-api/docs/caching)). agy's transcripts
  record each model call's `input_tokens`, `cache_read_tokens` and
  `created_at`, so the cache's behaviour was measured instead (8,624 calls
  over 14 conversations, 2026-11-04). The chance a call's input was cached,
  by the time since the previous call:

  | Gap | under 1 min | 1–5 min | 5–15 min | 15–30 min | 30–60 min | over 1 h |
  |---|---|---|---|---|---|---|
  | Cache hit | 97–99% | 85–89% | 50–57% | 31% | 8% | ~0% |

  So the cache fades rather than expiring: from agy's last model call
  (`AgyMeta.LastModelCallAt`, any planner response) it is shown `warm?` for
  5 minutes (`core.AgyWarmFor`), `cooling?` until 30 (`core.AgyColdAfter`),
  and after that the session is COLD (`cold 2h?`). The thresholds are fixed;
  Google can change the cache's behaviour, and the table can be re-measured
  from the same transcripts.
* **Context**: input + cache read + cache creation tokens of the last
  main-thread request.

Display groups, in order:

| Group | Contains |
|---|---|
| **NEEDS YOU** | needs_approval, needs_input, error |
| **YOUR TURN** | idle with an unseen completion |
| **WORKING** | working |
| **IDLE** | idle, cache warm — sorted by soonest cache expiry ("use it or lose it") |
| **COLD** | idle, cache expired |
| **UNKNOWN** | unmanaged sessions whose state can't be inferred |
| **CLOSED** | no process: every conversation on disk, newest first (see [conversations.md](conversations.md)) |

### 4.5 Merge rule: latest observation wins

Every source that observes activity produces `(timestamp, activity, detail)`.
For a Claude session with both hooks and a registry entry, the activity is the
most recent observation of the two; detail (tool, question, message) comes
from the most recent hook event consistent with that activity. Examples:

* Hook `PermissionRequest` at t1, then registry `waiting/permission prompt` at
  t1+ε: needs_approval, tool from the hook.
* Hook `PreToolUse` at t1, user presses Esc, registry `idle` at t2 > t1:
  idle. No Stop hook fires, so without the registry this would be stuck as
  "working"; this is the reason both sources are used.
* User denies permission manually: no hook, registry goes `busy` or `idle`.

For a session with hooks but no registry (agy), the hook state stands alone.
agy approval waits are not detected (see `docs/agy-hooks.md`).

Unmanaged Claude sessions: activity is inferred from the transcript tail
(last main-thread assistant message: `stop_reason` `end_turn` → idle;
`tool_use` with no following tool result → working, or needs_approval if the
pane has not produced output for a while) together with the pane's last-output
time. Shown with the `~` marker.

### 4.6 agy integration specifics

The agy plugin is installed under `~/.gemini/config/plugins/cccp/` with
`plugin.json` and `hooks.json` at the plugin root (the documented requirement
for native agy plugins). It hooks `SessionStart`, `PreInvocation`,
`PostToolUse` and `Stop`, and prints `{}` (agy hooks are synchronous and
expect JSON on stdout). `PreToolUse` is deliberately **not** hooked: its
output contract has a required permission `decision`, and a monitoring hook
must not be able to change permission behaviour. See `docs/agy-hooks.md` for
the empirical findings and consequences.

### 4.7 tmux integration (`integrations/tmux/cccp.tmux`)

* `set-hook -g NAME[97]` for `client-session-changed`, `session-window-changed`,
  `window-pane-changed`, `client-focus-in`, `client-focus-out`,
  `client-attached`, `client-detached`, `after-new-window`,
  `after-split-window`, `pane-exited`, `session-created` and `session-closed`
  → `run-shell -b 'cccp notify tmux <event>'`, which writes one line to the
  daemon socket and exits (a no-op if the daemon isn't running). The fixed
  index `[97]` leaves the user's own hooks (normally index 0) alone and makes
  re-sourcing idempotent. `pane-title-changed` is deliberately not hooked
  (see section 9).
* **Focus.** `focus-events on`, so "seen" and "don't notify" can require
  the terminal to have focus. tmux 3.4 starts every client flagged
  `focused` (`server-client.c`), clears the flag only when the terminal
  reports losing focus (`ESC[O`, `tty-keys.c`), and asks a terminal for
  reports (`ESC[?1004h`) only in `tty_update_features()`, on attach, if
  `focus-events` is on then (`tty.c`; the manual: "attached clients should
  be detached and attached again after changing this option"). So a client
  attached before the option came on stays "focused" forever. Found
  2026-11-03: three terminals attached in September, before cccp's
  installer turned the option on, made the current window of every
  attached session count as watched, so turn ends there (an agy pane, in
  the case reported) were marked seen instead of notified. The snippet
  therefore runs `cccp tmux-focus-since` before turning the option on; it
  sets `@cccp_focus_since` (once per server) to the server's start time if
  no session exists yet (loading at server start: every client attaches
  later), else to now (clients already attached may never have been
  asked, and there is no way to tell). A client counts as looking only if
  it attached at or after that time and is flagged focused
  (`model.TmuxClient.Looking`); the others count as not looking (a
  needless notification beats a missed one). `list-clients` returns
  `client_created`, `focus-events` and `@cccp_focus_since` with each client,
  so this costs no extra command. The installer and `cccp doctor` name the
  clients to reattach.
* Key bindings (root table, no prefix): `Shift+PgDn` → jump to the most
  urgent session needing you (repeat to cycle); `Shift+PgUp` → cockpit popup;
  `Shift+Up` → back to where you were before cccp's last jump (repeat to
  toggle; `internal/nav`: pressing `Shift+PgDn` repeatedly keeps the return
  point, state in a per-client tmux option).
  The keys a binding replaces are saved per table and restored on uninstall;
  installs that bound `prefix a`/`prefix A` are migrated on reinstall.
  VTE-based terminals may keep Shift+PgUp/PgDn for their own scrollback.
  (`display-popup -E -w 90% -h 85% "cccp ui --client '#{client_name}'"`).
* Status line: `#{@cccp_status}` in `status-right`; window list:
  `#{@cccp_glyph}` in `window-status-format` / `window-status-current-format`.
  The daemon writes these options; tmux reads them on its normal redraw, so
  no process is spawned per redraw.

### 4.8 Summaries

Free and exact: Claude `ai-title`, agy summary title, last prompt, last
assistant message (from the Stop hook), pending tool or question. The cockpit
shows these.

Optional one-line "what it needs from you" summary, generated by
`claude -p --model haiku --no-session-persistence` with `CCCP_INTERNAL=1` (so
cccp's own hooks ignore it), from the last prompt and last assistant message,
when a session newly needs you. One job at a time, cached by input hash,
60 s timeout. Config `summaries = "off" | "ac" | "always"`; default `"ac"`
(only on mains power). It uses the user's Claude subscription.

### 4.9 Search

`cccp search` and the cockpit's `/` (after a pause in typing) search the full text of all
transcripts (Claude: user prompts, assistant text, `ai-title`; agy: user
input and planner responses; tool output is excluded, because it dominates
the bytes and drowns the signal). Text is extracted incrementally (by byte
offset) into `~/.cache/cccp/text/`, then searched with a direct
case-insensitive substring/regex scan. At this corpus size (47 transcripts,
375 MB raw, an order of magnitude less after extraction) a scan is
milliseconds, so no index engine is warranted. The same incremental pass
records each conversation's catalog facts (title, cwd, branch, prompts,
turns), from which the CLOSED rows are built. In the cockpit, text matches
are listed after the fuzzy matches on the sessions' own facts
([conversations.md](conversations.md)).

### 4.10 Cockpit (`cccp ui`)

Bubbletea v2 + Lipgloss v2 + Bubbles v2. Talks to the daemon over the
socket; if the daemon is unreachable it gathers and computes in-process
(reading, never writing, the daemon's memory), so it always works.

* Header: counts per group, power state.
* Grouped list; row = state glyph, agent badge, tmux location, title, project,
  time in state, cache countdown, context size (closed: turns), detail (tool /
  question; closed: summary; search: the matching text).
* Preview: live `capture-pane` of the selected pane (refreshed only while
  the cockpit is open), or the conversation view (last prompt, last message).
* Keys: `enter` jump or resume · `tab` next needing attention · `/` search
  (one ranked list) · `a` set alias · `m` mark seen · `e` hide/show closed ·
  `p` toggle preview mode · `r` rescan · `q` quit.

## 5. Files and paths

| Path | Owner | Content |
|---|---|---|
| `~/.local/state/cccp/sessions/*.json` | hook | hook-observed per-session state |
| `~/.local/state/cccp/daemon.json` | daemon | seen-times, aliases |
| `~/.local/state/cccp/summaries.json` | daemon | summaries, keyed by session and input hash |
| `$XDG_RUNTIME_DIR/cccp.sock` | daemon | IPC (JSON lines) |
| `~/.cache/cccp/text/` | catalog | extracted transcript text + offsets, catalog facts per conversation; `.lock` serialises updates |
| `~/.config/cccp/config.toml` | user | options |

## 6. Failure modes

| Failure | Behaviour |
|---|---|
| daemon down | hooks still record state; `cccp ls`/`ui` compute in-process; tmux status shows the last value written (the daemon clears its options on clean shutdown). |
| hook binary missing/broken | Claude shows a hook error line; never blocks (always exit 0). |
| registry format changes | parsed leniently; unknown statuses map to `unknown`; hooks still work. |
| tmux server restarts | pane ids change; panes are rediscovered on the next tmux poke or refresh. |
| terminals attached before `focus-events` was on | they never report focus; cccp counts them as not looking (notifications while you watch them) and `cccp doctor` names them for reattaching (4.7). |
| several tmux servers | cccp shows the server it talks to; sessions in another server are listed, alive, without a pane (pane ids are compared with their server). |
| pid reuse | the process start time is checked against the recorded one. |
| clock skew between sources | all sources are on the same host clock. |
| state dir grows | ended sessions older than the retention period (default 7 days) are pruned by the daemon. |

## 7. Install

`cccp install` (prints each step and asks before modifying anything):

1. Claude plugin: this repo is a local marketplace (`.claude-plugin/marketplace.json`);
   `claude plugin marketplace add <repo>` + `claude plugin install cccp@cccp`.
   Takes effect for sessions started (or `/reload-plugins`) afterwards.
2. agy plugin: copy `integrations/agy-plugin` → `~/.gemini/config/plugins/cccp`.
3. systemd user unit `cccp.service`, enabled and started.
4. tmux: a `source-file` line for `integrations/tmux/cccp.tmux` appended to `~/.tmux.conf`.

## 8. Battery

* No timers except the single earliest-deadline timer; no polling of
  `claude agents --json`, tmux, transcripts or /proc.
* tmux status reads a user option on its existing redraw; no `#()` command.
* Transcripts are read only when the session's state changes, from the last
  read offset.
* The cockpit's live preview and countdowns update only while it is open.
* Power state (`/sys/class/power_supply/*/online`) is re-read when events
  arrive, not polled; on battery, summaries default to off.
* Acceptance check: daemon context switches over 5 idle minutes (from
  `/proc/<pid>/status`) should be a handful; under load they should track the
  event rate.

## 9. Implementation status (v1, 2026-11-02)

Built as designed, with these differences and measured results.

### Differences from the design above

* **`pane-title-changed` is not hooked.** Whether Claude Code animates the
  terminal title while streaming couldn't be ruled out; if it does, that hook
  would spawn a process per frame. Unmanaged sessions are rediscovered on
  focus/layout events, on cockpit refresh and at daemon start instead.
* **agy `PreToolUse` is not hooked** (docs/agy-hooks.md): agy's handling of
  a missing decision is undocumented and version dependent, and a failed hook
  is reported to deny the tool call.
* **agy pane output is not used for inference**: agy redraws its pane
  continuously even when idle (its window activity is always recent), so only
  its transcript is used.
* **Title choice**: custom title (`/rename`, `--name`) → registry name if
  not derived → transcript `ai-title` → agy summary title → pane title
  (Claude sets `✳ <title>`) → registry derived name → cwd basename.
* **Completion seen-marks** are only written when a completion actually
  becomes seen (not on every snapshot), so daemon.json is rewritten rarely.
* **Summaries** run `claude -p --model haiku --no-session-persistence
  --setting-sources "" --strict-mcp-config --tools ""` with stdin closed
  (about 2 s per summary in testing).
* **Hooks are synchronous** (not `async: true`): synchronous hooks keep
  `PreToolUse → PermissionRequest → PostToolUse` ordered, which an
  attention monitor depends on. Measured cost: 3.7 ms per invocation
  including the `sh` guard (100 invocations in 0.37 s).

### Verified

* Unit tests: reducer event sequences (normal turn, permission then approval,
  out-of-order async notification, interrupt corrected by `idle_prompt`,
  `AskUserQuestion`, `StopFailure`, subagent `Stop`, compaction, end/resume,
  agy turn/cancel/error); hook payload parsing (documented Claude payloads,
  an agy payload captured live); transcript reader (window growth, partial
  final line, pending tool, interrupt, TTL bucket, sidechain exclusion);
  core merge rule, grouping, seen/unseen, pid supersession, registry
  busy→idle completions; store lock under 50 concurrent writers; search
  extraction (tool output excluded, incremental); `/proc` stat parsing.
* Daemon integration test: the real daemon over a temp state dir; a hook
  state write reaches a socket subscriber via inotify; request/response ops.
  Race detector clean on every package.
* End to end with a real `claude -p` run using the plugin from a scratch
  directory (`--plugin-dir`): prompt, tool call, final message, turn end
  and session end recorded, with the agent's pid and tmux pane.
* Live discovery on the development machine: all 30 agent panes found (23
  Claude: 16 via the registry and 7 unmanaged; 7 agy, all unmanaged), with
  distinct conversations for agy processes sharing a directory.
* Install/uninstall round trip in a sandbox HOME against an isolated tmux
  server: options with quotes and existing key bindings restored exactly,
  hooks removed, the tmux.conf line removed.
* Battery: under strace, steady state after startup is one Go runtime timer
  (the debounced state save), then the scheduler parks for 60 s and the
  netpoller and the pidfd epoll wait indefinitely. In the first minutes after
  start, Go's background scavenger returns memory from the initial transcript
  scans, which shows up as ~1–1.5 context switches/s and decays to the
  floor.

### Known limitations

* Sessions whose transcripts were deleted by Claude Code's
  `cleanupPeriodDays` (long-lived sessions older than the period) and that
  predate the registry show as UNKNOWN with only their pane title.
* An unmanaged Claude session with a pending tool and a quiet pane is shown
  as needing approval (inferred); a tool that runs silently without Claude's
  spinner would be misread. Hooked and registry sessions are not affected.
* Multi-pane windows: tmux tracks output activity per window, so the
  quiet-pane inference is disabled for panes in split windows.
* `claude -p` runs started from inside a tmux pane are real sessions with
  hooks and appear (briefly) in that pane's location.

### Changes after first use (2026-11-02)

* **Print-mode runs are not sessions.** `claude -p` registers in Claude's
  session registry as `kind: "interactive"` with `entrypoint: "sdk-cli"`, and
  with the plugin installed its hooks fire too. Such runs (scripts, other
  tools) were listed briefly and notified "your turn" when they exited. They
  are now excluded at every source: registry entries with an `sdk*`
  entrypoint, hook sessions whose `claude`/`agy` process was started with
  `-p`/`--print`/`--prompt` (recorded by the hook as `print_mode`), and
  print-mode processes found under tmux panes.
* **Summary scheduling** (`internal/daemon/sumqueue.go`): at most one queued
  job per session (the newest input replaces an older queued one); a failure
  backs that input off for 1 min, doubling to 1 h; 3 consecutive failures
  across sessions (logged out, usage limit) pause all summaries for 15 min.
  Before this, a failing `claude -p` was respawned on every recompute.
* **Notifications are logged** (`journalctl --user -u cccp`) with session,
  group and location. (The original daemon integration test used the default
  config and sent a real notification on every `go test` run; on mains power
  it would also have run a real `claude -p`. Section 10 describes the
  architecture that replaced it.)

## 10. Code architecture: functional core, imperative shell

Rewritten 2026-11-02 after the test leaks above. The rule: every decision is
made by a pure function or a deterministic state machine that takes all its
inputs as values; the code that touches the world is thin enough to check by
reading, and is reached only through interfaces.

### Packages

| Package | Kind | Role |
|---|---|---|
| `model` | pure | data types: hook state and events, registry entry, transcript metadata, tmux panes and clients, agy conversations |
| `reduce` | pure | hook events → `HookState` (a state machine per session) |
| `core` | pure | `Plan(Observation) Needs`; `Compute(Observation, Memory, Config, watchStart, now) (Snapshot, Memory)`; `NextAttention`, `PlanJump`, `NextDeadline`, `Expired` |
| `machine` | pure | the daemon as a state machine: `Step(Input) []Effect`; notification de-duplication, tmux diffing, summary queue and backoff, save debounce, timer, pruning |
| `present`, `config` | pure | formatting; configuration type, defaults and `Parse` |
| `profile`, `fuzzy` | pure | Claude profile resolution; fuzzy matching for the cockpit's search |
| `gather` | shell | builds an `Observation` from interfaces (`HookStore`, `Registry`, `TmuxReader`, `Procs`, `Transcripts`, `Files`, `AgyDB`, `Power`), refreshing only what an event made stale |
| `daemon` | shell | event loop: OS events → machine inputs; effects → ports (`TmuxWriter`, `Notifier`, `Summarizer`, `MemoryStore`, `FileRemover`, `Indexer`) |
| `hook` | shell | `Handle(Deps, agent, stdin, now)`; payload parsing is pure |
| `install` | shell | install/uninstall through a command runner and a tmux runner; `tmux.conf` edits are pure |
| `store`, `claudereg`, `transcript`, `agydb`, `procs`, `tmux`, `power`, `memfile`, `catalog`, `ipc` | adapters | one external resource each |
| `ui` | Elm architecture | Bubble Tea model over `Source`, `Actions`, `Searcher`, `PaneCapturer` interfaces |
| `paths` | pure | `Resolve(getenv, home, uid) Layout` |
| `jail` | pure | cgroup limits and placement: unit names, presets and boosts, `Desired`, `Placement`, D-Bus property encoding |
| `cgroups`, `systemd` | adapters | the cgroup tree under `/sys/fs/cgroup`; the systemd user manager over D-Bus |
| `cmd/cccp` | composition root | the only code that reads the environment and constructs real adapters |

### Rules, and how they are enforced

* **Pure packages perform no I/O and depend on no adapter.**
  `internal/archtest` parses their imports and fails on any I/O package (`os`,
  `net`, `syscall`, `log`, …) or any cccp package outside the allowed set.
* **No global state, no environment reads below the root.** Paths, sockets
  and the tmux server are values passed in; `paths.Resolve` is called once,
  in `cmd/cccp`.
* **Real adapters refuse to act inside a test binary.** The notifier, the
  summarizer and a tmux client for the default server return an error under
  `testing.Testing()`, so a test that forgets a fake fails instead of
  touching the desktop, the user's tmux or their Claude subscription.
* **The core is deterministic.** Maps are iterated in sorted order and ties
  are broken explicitly; `TestComputeIsDeterministic` and the machine's
  `TestDeterministic` check it.

### Test tiers

1. **Pure unit tests** (the bulk): `reduce`, `core`, `machine`, `present`,
   `config`, `paths`, `install`'s conf edits. Literal values in, values out.
   The core and machine rules were mutation-tested: each rule was broken
   on purpose and the suite had to fail. Every mutant was killed except one
   equivalent mutant (`outKnown &&` in the quiet-pane inference: an unknown
   output age is zero, which is never at or past the threshold).
2. **Shell logic with fakes**: `gather` (bookkeeping, rediscovery rules,
   which transcripts are read, the miss cache), `daemon` (real fsnotify,
   socket and timer in a temp dir; every port faked), `hook.Handle`,
   `install` (commands issued, files written, the restore round trip), `ui`
   (keys, filter, search, jump, preview).
3. **Adapter contract tests against sandboxed real resources**: `store`
   (50 concurrent writers under the lock), `transcript` (windowed backward
   scan, partial lines), `search` (extraction, incremental updates), `procs`
   (its own process), `memfile`, and the pidfd watcher (a real child
   process killed while watched), and `tmux`: a workstream restore plan run
   against a real tmux server on a socket in a temp directory, configured
   with `-f` (never `~/.tmux.conf`), checking windows, layouts, directories
   and resume marks; and `cmd/cccp`'s move action on the same kind of
   sandbox (into an existing, a new and a dormant workstream, and a closed
   conversation).
4. **Manual end-to-end** (not in `go test`, because they cost money or touch
   the user's environment): a `claude -p --plugin-dir` run with a scratch
   copy of the plugin, the cockpit rendered in an isolated tmux server
   (`tmux -L`), and the installed daemon checked with `cccp doctor`.

`go test -race ./...` runs tiers 1 to 3 in a few seconds, with no network,
no subprocess other than `sleep`, `sh` and a sandboxed `tmux` (skipped where
tmux is not installed), and nothing outside temp directories.

The workstream flows above the adapter (`cccp start`, the placeholder, the
cockpit's `w`, closing and forgetting with a real daemon) were checked end to
end in tier 4: an isolated tmux server and daemon (their own state, cache,
socket and config, notifications and summaries off), with a stand-in
`claude` on PATH; docs/workstreams.md lists what was shown.

## 11. Profiles (2026-11-02)

A **profile** is a Claude Code config directory (`CLAUDE_CONFIG_DIR`); each
has its own registry (`<dir>/sessions`), transcripts (`<dir>/projects`),
plugins and account. The user runs a personal profile (`~/.claude`) and a
work profile (`~/.claude-work`, via an alias) on different organisations'
subscriptions.

* **Resolution** (`internal/profile`, pure): the default profile is fixed at
  `~/.claude`, because a session that reports no `CLAUDE_CONFIG_DIR` is
  there regardless of cccp's own environment; configured `[[profile]]`
  entries add names and per-profile `summaries`; discovered directories
  (running processes' environment, `~/.claude*` directories containing
  `sessions/` or `projects/`, hook state) complete the list. Unconfigured
  names are derived (`~/.claude-work` → `work`) and kept unique.
* **Attribution** (`core`): hook state records the hook's inherited
  `CLAUDE_CONFIG_DIR`; a registry record carries the profile whose registry
  holds it; an unmanaged process's environment is read. Transcript lookups
  and title matching happen within the session's profile only
  (`ProjectKey{ConfigDir, Cwd}`); a session with the same title in another
  profile never matches.
* **Watching** (`daemon`): every profile's registry is watched, including
  profiles discovered while running.
* **Summaries** (`machine`, `daemon.ClaudeSummarizer`): `StartSummary`
  carries the session's profile (agy sessions use the default), the policy
    is resolved per profile, and the adapter selects it explicitly
  (`model.Profile.Environ`, below). Verified before implementing: with the summarizer's flags
  (`--setting-sources ""`), credentials come only from `CLAUDE_CONFIG_DIR`;
  an empty config directory refuses with "Not logged in", and each real
  profile answered on its own account.
* **Jumping** (`core.PlanJump`): resuming or attaching runs in the
  session's profile, whatever the tmux server's environment: another
  profile with `-e CLAUDE_CONFIG_DIR=…` on the new window, the default one
  as `env -u CLAUDE_CONFIG_DIR claude …`.
* **Selecting the default profile** (`model.Profile.Environ`, used by every
  path that starts `claude`): the variable is removed, never set to
  `~/.claude`. Claude Code puts its global state at
  `join(CLAUDE_CONFIG_DIR || homedir, ".claude.json")` but its settings at
  `join(CLAUDE_CONFIG_DIR || ~/.claude, "settings.json")` (read from the
  2.1.317 bundle), so `CLAUDE_CONFIG_DIR=~/.claude` keeps settings and
  credentials but reads a blank `~/.claude/.claude.json` and runs the
  first-start flow. Until 2026-11-02 cccp did set it, in resume/attach,
  summaries, install and doctor; that created a stray
  `~/.claude/.claude.json`, which nothing reads now.
* **Install / doctor**: the plugin is installed into, and checked in, every
  profile; doctor shows each profile's account (`claude auth status`).

Correction recorded here: the seven work-profile sessions earlier shown as
UNKNOWN, with a note blaming Claude Code's transcript cleanup, were work
profile sessions whose registry and transcripts cccp was not reading. The
note now says only "no transcript found".

## 12. Closed conversations (2026-11-02)

Every conversation on disk, in any harness and profile, is a row: those
without a process are CLOSED, below COLD. `/` searches all rows in one ranked
list; `enter` on a closed row resumes it in a new tmux window. Design, data
locations and battery behaviour: [conversations.md](conversations.md).

## 13. Clicking a notification (designed, not built)

Going to a notification's session on click needs the terminal to raise the
right window, which on GNOME Wayland only the terminal itself can do. The
design uses Ghostty ≥ 1.4.0 (`GHOSTTY_SURFACE_ID` and its `present-surface`
D-Bus action) and D-Bus notifications from the daemon. It waits for that
release: [notification-click.md](notification-click.md).

## 14. Workstreams (2026-11-04)

A workstream is a tmux session. `w` in the cockpit creates one (a name, then
a directory picked fuzzily or typed, made if new); closing its last window
closes it, and cccp forgets it once the same server has run without it for
10 s (a shutdown, which kills the server, forgets nothing). The daemon
records every workstream's windows, layouts, directories and conversations;
`cccp start` restores them after a reboot, conversations as placeholders
that resume in their own profile on Enter. Design, tmux facts and tests:
[workstreams.md](workstreams.md).

## 15. Jails (2026-11-05)

tmux runs in `cccp.slice` and each workstream in its own slice under it, so
a runaway cannot exhaust the machine's memory or starve the desktop or the
other workstreams. `core` computes the desired limits and placement from
the observation (`jail.Desired`, `jail.Placement`); the machine applies
what differs and moves what is misplaced (`JailLimits`, `JailMove`,
retried after a minute on failure), watches kills and use near the limit
(notifications), and keeps boosts in `Memory` with their expiry as a
deadline. The daemon talks to systemd (`StartTransientUnit`,
`SetUnitProperties`, `AttachProcessesToUnit`) and polls the slices every
15 s. The limit is a hard `MemoryMax`, not a throttle point: one was
measured to hang a runaway indefinitely. Design, facts and measurements:
[jails.md](jails.md).
