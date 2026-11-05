# Jails: memory and CPU limits for workstreams

Status: built, 2026-11-05. Design and decisions the same day; the memory
limit changed from throttle-then-kill to a hard limit after testing (below).

## Goal

An agent that runs something heavy (a build, a test run, a runaway script)
must not make the machine unresponsive: not by exhausting memory and
swap, and not by starving the desktop of CPU. And one workstream must not
starve another. When a workstream needs more for a while, that is one key
in the cockpit, and it reverts by itself.

## Mechanism: cgroups through systemd

The user's systemd manager owns a subtree of the cgroup hierarchy (cgroups
v2) and delegates the `cpu`, `memory` and `pids` controllers to it. In
systemd terms a **slice** is a node of that tree that holds other units
and carries limits; a **scope** holds processes started by something else.
Limits on a slice apply to everything under it together, and a process's
children are always created in its cgroup, so nothing an agent starts
escapes its pane's cgroup, not even by detaching (`nohup`, `setsid`). The
exception is containers started through the Docker daemon, which runs them
in its own cgroups.

Two levels:

```
user@UID.service                    the user's systemd manager
├─ app.slice                        the desktop's apps
└─ cccp.slice                       A: all of tmux, beside the desktop
   ├─ cccp-tmux.scope               the tmux server
   └─ cccp-ws.slice
      ├─ cccp-ws-infra.slice        B: one workstream's own limits
      │  └─ cccp-pane-….scope       its panes, and everything run in them
      └─ cccp-ws-docs.slice
```

Workstream names are escaped for unit names (`systemd-escape` rules: `-`
becomes `\x2d`, so a name's dashes never read as levels).

## Facts this rests on

Tested on this machine (systemd 255, cgroups v2, Linux 6.17), 2026-11-05:

* The user manager delegates `cpu memory pids`; `io` is not delegated, so
  disk I/O cannot be limited without a system-wide change (left out).
* A running process can be moved into a new scope in a named slice with
  the manager's `StartTransientUnit(name, mode, [Slice, PIDs])`; it left its
  tmux pane scope and its later children are created in the new one.
* Slice limits set at runtime (`SetUnitProperties`, or `systemctl --user
  set-property --runtime`) take effect at once on everything running;
  percentages are of physical memory (`MemoryHigh=5%` → 4.6 GB of 86 GB).
* Dash names nest: `cccp-ws-a.slice` → `cccp-ws.slice` → `cccp.slice`, a
  sibling of `app.slice`.
* `memory.current`, `memory.events` (`high`, `max`, `oom_kill` counts) and
  `cpu.stat` (`usage_usec`) are readable in each slice's directory.
* tmux 3.4 puts each new pane in a `tmux-spawn-….scope` in the **server's
  slice** (`compat/systemd.c`: "inherit the slice from the parent
  process"), so with the server in `cccp.slice`, every new pane starts under
  A.
* Moving a process does not move the memory already charged to it (cgroups
  v2): memory a process allocated before it was moved stays charged where
  it was until freed. New allocations, and every new subprocess, are charged
  to the jail.
* **Stopping a slice kills everything in it** (`StopUnit` on a slice stops
  its scopes). cccp never stops one; a workstream's slice stays, empty, after
  the workstream closes, and costs nothing.
* **A throttle point hangs a runaway instead of stopping it.** With
  `MemoryHigh` 48M, `MemoryMax` 64M and no swap allowed, a process
  allocating without end stopped at 57.6 MB and stayed there, frozen in
  reclaim, holding its memory, for as long as it was watched: never reaching
  the kill point. `systemd-oomd` did not kill it either: it picks among
  cgroups the kernel is scanning for reclaim (`Pgscan`), and reported 0.
  Hence the hard limit alone (Limits).
* **systemd stops a whole unit after a kill in it, by default.** With
  `OOMPolicy=stop` (the default), a kill at the limit made systemd stop the
  pane's scope: SIGTERM at once to everything in the pane (an agent exits on
  it), SIGKILL 90 s later to what ignored it (the shell), and the unit left
  `failed` (the user manager `degraded`). cccp makes every scope with
  `OOMPolicy=continue`: the kernel kills the one process (the scope's
  `memory.oom.group` stays 0), and the agent and shell beside it carry on
  (measured: the hog killed, an agent stand-in that exits on SIGTERM still
  running, the scope active with `Result=success`).

## Limits

Presets (percentages of physical memory; on 86 GB: 47% ≈ 40 GB):

| | `MemoryMax` (the limit) | `MemorySwapMax` | `CPUWeight` | `TasksMax` |
|---|---|---|---|---|
| **A** `cccp.slice` | 80% | 2G | 50 (the desktop's apps: 100) | |
| **A** under `max` | 90% | 4G | 200 | |
| workstream `normal` | 47% | 1G | 100 | 4096 |
| workstream `boost` | 80% | 2G | 400 | 8192 |
| workstream `max` | 90% | 4G | 400 | 8192 |

`MemoryHigh` is set to `infinity` everywhere (it can still be set in the
config, for anyone who wants it).

* **A hard limit.** At `MemoryMax` the kernel kills a process inside the
  workstream (its choice, usually the largest), at once, not the browser;
  `MemorySwapMax` keeps the workstream from swapping the machine to a
  standstill on the way. There is no throttle point before it: one hangs a
  runaway rather than stopping it (Facts). The warning comes from cccp
  instead (Visibility).
* **CPU weight is relative to siblings.** Among workstreams, `boost` (400)
  gets four times a `normal` one's share when they compete. Against the
  desktop only A's weight counts, which only `max` raises: `max` lets that
  workstream (and, while it lasts, A as a whole) outrank the desktop.
* `custom` sets any of these per workstream.
* Defaults and per-workstream settings are in cccp's config; the presets'
  values can be changed there.

## Boosting at runtime

`L` in the cockpit, on any of a workstream's rows (or `cccp limits NAME
PRESET [--for 2h | --until-undone]`): choose `normal`, `boost` or `max`,
and for how long (1 h default, 2 h, 8 h, until undone). The daemon applies
it at once and reverts it when it expires (a timer deadline, like a cache
expiring). Boosts are the daemon's state, with their expiry, persisted; an
expired one never comes back, and an "until undone" one is applied again
whenever its slice is recreated (after a reboot, by `cccp start`).

Lowering a workstream below what it uses (a limit under its current
memory) makes the kernel kill a process in it at once: the cockpit shows
each choice's limit and warns on one below current use.

## Placement: reconciliation

cccp decides where each pane's processes should be, compares with where
they are, and moves what is in the wrong place, the same way it keeps the
workstream record:

* the tmux server: in `cccp-tmux.scope` (A);
* each pane: in its workstream's slice. A pane whose cgroup is elsewhere
  (a new pane, which tmux puts in `cccp.slice`; a window moved with `M`; a
  restored pane; every pane at the first install) has all the processes in
  its cgroup moved into a new `cccp-pane-….scope` in the right slice, and
  any that appeared meanwhile are moved after them (`AttachProcessesToUnit`),
  until the old cgroup is empty.

Nothing changes how a pane starts: tmux starts the user's shell as always,
so a broken cccp never stops a pane from opening. The first moments of a
new pane, before it is moved, count against A.

No restart is needed when this is installed: the running tmux server and
every pane are moved. Only the memory existing processes allocated before
the move stays charged to their old cgroup (above): restart a session when
convenient for it to be counted fully.

Each slice's limits are applied when it is created and whenever the
desired limits change (a boost, an expiry, a config change); the daemon
keeps what it last applied and changes only differences. Every move also
sets A's and the target slice's limits again just before it: if someone
stopped the slice, the move would otherwise create it again implicitly,
without limits.

A failed limit or move is retried after a minute. After a move or a limit
change the daemon waits for systemd's job and looks again, so the cockpit
shows the new place and limits at once rather than at the next event.

## Visibility

* The cockpit reads each workstream's slice while it is open (every 2 s):
  memory use against its limit, and CPU use (from `usage_usec` between
  reads).
* The workstream column shows `⇧` for a boosted workstream and turns yellow
  at 85% of its limit; the preview shows the numbers and the boost's time
  left.
* The daemon reads the running workstreams' slices every 15 s (a few small
  files each), and watches each slice's `memory.events`. Two desktop
  notifications:
  * at 85% of a workstream's limit, "cccp: infra is at 85% of its memory
    limit", once; again only after use has fallen below 75%;
  * a kill at the limit (`oom_kill` rising): "cccp: infra hit its memory
    limit". Kills that happened before the daemon started are not reported,
    and a slice made again (its counters start at 0) reports only kills
    after that. A slice that does not exist yet reads as zero, so a kill
    seconds after it is made is reported too.
* `cccp limits` prints every workstream's preset, use, limit, CPU weight and
  kills; `cccp doctor` says whether jails are on, and any process not yet
  where it belongs.

## If systemd is not there

No user manager, no cgroups v2, or controllers not delegated: jails are
off, `cccp doctor` says why, and everything else works as before.
