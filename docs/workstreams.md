# Workstreams

Status: built 2026-11-04. Code: `internal/workstream` (the record, the
forgetting rule, the restore plan), `internal/dirpick` and `internal/dirs`
(the directory picker), `cmd/cccp/workstreams.go` (`cccp start`, `cccp
resume-pane`, the cockpit's actions), `internal/ui/create.go` (the `w`
flow).

## Goal

One terminal. Work is grouped into **workstreams**; you switch between them
from the cockpit (or with the jump keys), and after a reboot one command
brings them all back.

* A **workstream is a tmux session.** Every tmux session counts, whether it
  was created from the cockpit or with `tmux new`.
* Inside a workstream you use tmux as usual: new windows, splits, `claude`,
  `agy`, shells.
* Closing a workstream's last window closes the workstream, and cccp forgets
  it. Closing the last workstream leaves tmux.
* Nothing starts by itself: the terminal opens bare. After a reboot,
  `cccp start` restores the workstreams, attaches and opens the cockpit.

## tmux facts this rests on

Tested on tmux 3.4 (isolated servers, an attached terminal driven by
`script`), 2026-11-04:

* Closing a session's last window destroys the session; `session-closed`
  fires with `#{hook_session_name}`.
* With tmux's default `detach-on-destroy on`, the terminal that was showing
  that session **drops out of tmux**. With `off`, `previous` or `next` (all
  accepted by 3.4) it switches to another session; with none left it leaves
  tmux either way.
* **`session-closed` also fires when the server shuts down**, on
  `kill-server` and on SIGTERM (logout, reboot), unreliably: only some
  sessions' hooks ran. So the hook cannot tell "you closed it" from "the
  machine is going down".
* A server is identified by its pid, `#{start_time}` and socket; sessions by
  `#{session_id}` (`$0`), stable across `rename-session`, which fires
  `session-renamed`.
* `#{window_layout}` (e.g. `b25d,80x24,0,0,0`) is a window's pane
  arrangement, reapplied with `select-layout`.
* Session names may contain `/` (`work/infra` stays as is); `.` and `:`
  become `_`.
* A command chained after `attach` runs as the attaching client:
  `tmux attach -t beta \; run-shell -b '… #{client_name} …'` saw the new
  client's name and session.
* With `exit-empty off` the server outlives its last session and still
  answers `display-message -p '#{pid} #{start_time}'`; `list-panes -a`
  then fails with "no current target", which means "no panes". With the
  default (`on`) the server exits.
* `select-layout` applies a layout recorded from another window: it ignores
  the pane ids in the string, assigns panes in order, and resizes.
* A per-pane user option (`set -p @cccp_resume …`) is readable in
  `list-panes` formats; `#{automatic-rename}` is readable per window.

## Creating: `w` in the cockpit

Two steps, `Esc` going back one:

1. **Name.** Prompt `workstream: `, Enter. An existing workstream of that
   name: switch to it (no second step). Names are validated before tmux sees
   them: empty, or containing `.` or `:`, is refused with a message (tmux
   would silently change them).
2. **Directory.** An fzf-style picker:
   * **Candidates**, existing directories only, deduplicated, shown with
     `~` for home:
     * known directories first, most recently used first: the working
       directories of every conversation cccp knows (live and closed, all
       profiles and harnesses), of every live tmux pane, and of recorded
       workstreams;
     * then directories under `$HOME` from a bounded walk, started when the
       picker opens, off the UI loop: depth ≤ 4, skipping hidden directories
       and dependency or build trees (`node_modules`, `target`, `vendor`,
       `__pycache__`, `.git`, …), capped at 20 000 entries. One walk per
       picker, nothing in the background.
   * **Matching:** the cockpit's fuzzy matcher over the path. When the input
     starts with `~` or `/`, it is a path: the picker lists the children of
     the deepest existing directory in it, filtered by the rest.
   * **Keys:** `↑↓` select; `Tab` copies the selection into the input with a
     trailing `/`, to append to it; `Enter` opens the selection.
   * **New directories:** when the input is a path that does not exist, the
     list ends with `+ create ~/git/newproj`; `Enter` on it creates it
     (`mkdir -p`, 0755) and opens it. It comes *after* the matches, so
     `Enter` on a near miss (`~/git/inf`) opens the match (`~/git/infra`)
     instead of creating a directory; with no match it is the only row.
     Creating is always that explicit row.
   * The default selection, with nothing typed, is `$HOME`.

Then `tmux new-session -d -s NAME -c DIR`, the client switches to it, and
the cockpit closes.

## Moving a session: `M` in the cockpit

`M` on a session row opens a picker of the other workstreams (fuzzy;
dormant ones marked, and restored first). A name matching none adds `+ new
workstream «name»` after the matches; Enter takes the highlighted row. The
cockpit stays open (a status line says what moved), so several sessions can
be sorted in a row; Enter afterwards jumps as usual.

What moves (`core.PlanMove`):

* a session in a tmux pane: its **whole window**, with whatever else is
  split beside it (`move-window`, everything keeps running);
* a background session: its `claude attach` window opens in the
  destination;
* a closed conversation: it resumes in a new window in the destination
  (its directory must still exist);
* a session open outside tmux: refused, as Enter refuses it;
* into its own workstream: refused.

The new window does not become the destination's current window (`-d`), so
nobody viewing it is moved. A new workstream starts in the session's
directory (where its new windows open); tmux cannot create an empty session,
so the window it starts with is closed once the moved one is in. Moving a
workstream's last window out closes that workstream, and the usual rules
apply (anyone viewing it goes back to the previous one; it is forgotten
after 10 s). The snippet hooks `window-linked` and `window-unlinked`, so the
record follows at once.

## Closing, and forgetting

Closing a workstream is ordinary tmux: close its last window (or
`kill-session`). The snippet sets `detach-on-destroy previous`, so the
terminal goes back to the workstream you used before; closing the last one
leaves tmux. It also sets `exit-empty off`: otherwise tmux exits when its
last session closes, which would look exactly like a shutdown (the server
gone) and bring the workstream you just closed back at the next `cccp
start`. With the server kept, closing the last workstream is "the same
server, the session gone" like any other. (The installer saves both
previous values and uninstall restores them; an upgrade saves an option the
first time it sets it.)

**Forgetting is decided by reconciliation, not by the hook.** Each recorded
workstream names the server it was last seen on (pid, start time, socket).
It is forgotten only when:

1. **that** server is still running, and
2. the session is gone from it, and
3. that is still true at a second observation **10 s later** (a pending
   removal, persisted).

A shutdown kills the server within that window (or kills the daemon, which
then finds the server gone and drops the pending removal), so a reboot never
forgets anything. `session-closed` is only a prompt to look (the daemon
already hooks it); a missed or spurious hook changes nothing. The cost:
closing a workstream and rebooting within 10 s brings it back once.

Windows and panes closed inside a running workstream are dropped from the
record by the same rule, without the delay: the server being up and the
session present is enough to know you closed them.

## Recording

The daemon already rereads tmux on every layout event (window and pane
created or closed, session created, closed or renamed). From each snapshot
it derives, purely (`core`), the record, saved to
`~/.local/state/cccp/workstreams.json` only when it changes:

```
workstreams: [
  {name: "infra", server: {pid, start_time, socket}, id: "$3",
   last_used: …,
   windows: [
    {index: 1, name: "claude", layout: "b25d,80x24,0,0,0", active: true,
     panes: [
       {cwd: "/w/infra", kind: "conversation",
        agent: "claude", session_id: "…", config_dir: "~/.claude-work"},
       {cwd: "/w/infra", kind: "shell"}]}]}]
pending_removals: [{name: "old", server: …, since: …}]
```

A workstream whose server is gone is **dormant**: kept as recorded until
`cccp start` restores it. Workstreams are keyed by name: a session you
create by hand on a new server under a dormant workstream's name replaces
that dormant record.

A pane's kind comes from what cccp already knows: a live Claude or agy
session in that pane is a conversation (its id and profile); anything else
is a shell in its directory. Other programs (an editor, a server) are
restored as a shell in their directory: re-running a recorded command line
is how tmux-resurrect restores programs, and it is wrong as often as right
(it is how a plain `claude` would come back as a new, empty conversation).

## `cccp start`

The one command after a reboot (and harmless at any time):

1. **Restore** every dormant workstream whose name is not taken on the
   running server, detached (starting the tmux server if there is none):
   * `new-session -d -s NAME`, the windows in their order and names, each
     window's panes split, then `select-layout` with the recorded layout;
   * a shell pane: a shell in its directory;
   * a conversation pane, **lazily**: a placeholder, `cccp resume-pane`,
     that prints `⏎ resume «title»` and the exact command. On Enter it runs
     the resume command `core.PlanJump` already builds (`claude --resume ID`
     with the profile selected, in the directory; `agy --conversation=ID`)
     as its child; when that ends the pane becomes a shell in its directory,
     as a pane where you had typed `claude` would. `Ctrl-D` or `Ctrl-C`
     gives the shell straight away. `cccp start` marks the pane with the
     conversation (the `@cccp_resume` option) as it creates it, so the
     record keeps the conversation while the placeholder waits (a reboot
     before you resume it loses nothing); the mark is cleared when the pane
     becomes a shell, and a live session in the pane outranks it.
2. **Attach and open the cockpit**:
   * outside tmux: `exec tmux attach -t LAST \; run-shell -b "cccp popup
     --client '#{client_name}'"`, LAST being the most recently used
     workstream;
   * inside tmux: open the cockpit on this client;
   * no workstream at all: the two `w` prompts (name, directory) run in the
     terminal, then it attaches to the new workstream.

The daemon never starts tmux or restores anything by itself.

`cccp start` restores into whatever tmux server is running, or a new one,
started with your `~/.tmux.conf` (which sources cccp's snippet).

Known limit: a server that dies sends no further event, so the daemon's
snapshot (and the cockpit) shows its workstreams as running until the next
tmux event. `cccp start` applies the rule against the actual server, so
what it restores is right regardless.

Caveats: resumed conversations start with a cold prompt cache; a turn that
was running at shutdown resumes as interrupted.

## Tests

As built, all passing; each rule's guard was mutated and the mutant caught:

* Pure: record derivation from a snapshot; the forgetting rule as a state
  machine (same server and gone twice 10 s apart → forget; server gone →
  dormant, pending removal dropped; gone then back → cancel); the restore
  plan from a record and the running server (name collisions, layouts,
  placeholders); name validation; the directory picker's candidate ranking
  and path mode; the create row.
* Isolated tmux servers, as in the experiments above: `w` with an existing
  and a new directory, closing the last window (forgotten after the delay),
  killing the server (dormant, nothing forgotten), `cccp start` into a fresh
  server (sessions, windows, layouts, placeholders, attach and popup).
