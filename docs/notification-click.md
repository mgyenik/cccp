# Click a notification to go to its session

Status: researched and designed 2026-11-03; **not built**. Waiting for Ghostty
1.4.0, which the installed package (Ubuntu PPA, `1.3.1-0~ppa2`) does not have
yet. Pick this up when `ghostty --version` reports 1.4.0 or later.

## Goal

Clicking a cccp desktop notification brings you to the session it is about:

1. the operating system raises the terminal window holding the right tmux
   client, and
2. that tmux client switches to the session's pane.

## Why it is not trivial (GNOME 46 on Wayland)

* **Raising another program's window is not allowed on Wayland.** `wmctrl` and
  `xdotool` need X11; GNOME 41 restricted `org.gnome.Shell.Eval`. A window is
  raised by its own application, ideally with an XDG activation token
  ([xdg-activation-v1](https://wayland.app/protocols/xdg-activation-v1)).
* **One terminal process owns every window.** Both GNOME Terminal
  (`gnome-terminal-server`) and Ghostty (single-instance GTK application) run
  all windows in one process, so process ancestry cannot tell which window
  holds which tmux client: every client's parent chain leads to the same pid
  (verified for GNOME Terminal on 2026-11-03: three tmux clients, one
  `gnome-terminal-server`, windows `/org/gnome/Terminal/window/{1,2,3}` on
  D-Bus, none exposing which pty it holds).
* **GNOME 46 sends no activation token for notification clicks.** Measured
  2026-11-03 with `dbus-monitor` on `org.freedesktop.Notifications` while a
  `notify-send -A default=Open -A jump=Jump` notification was clicked:
  `ActionInvoked(id, "default")` then `NotificationClosed(id, 2)`, and no
  `ActivationToken` signal (the signal is optional in the spec:
  [xdg-specs MR 43](https://gitlab.freedesktop.org/xdg/xdg-specs/-/merge_requests/43/diffs);
  libnotify can read one: [libnotify commit](https://lists.gnome.org/archives/commits-list/2022-April/msg08742.html)).
  So a program reacting to the click has no token to pass on.

## What Ghostty provides (read in its source)

* **`GHOSTTY_SURFACE_ID`**, from **v1.4.0**: every surface's child process
  gets its surface's id as `0x%016x` in the environment (`src/Surface.zig`).
  A tmux client started in a Ghostty surface inherits it, so
  `/proc/<client_pid>/environ` names the surface holding each tmux client.
  Not in v1.0.0–v1.3.1 (checked at each tag).
* **`present-surface`**, since v1.0: an application action on
  `com.mitchellh.ghostty` (`/com/mitchellh/ghostty`, `org.gtk.Actions`),
  parameter a `u64` surface id, which presents that surface
  (`src/apprt/gtk/class/application.zig`, `actionPresentSurface`; Ghostty's
  own notifications use it as their default action). Present on 1.3.1 here
  (`org.gtk.Actions.List` on the app object lists it); 1.3.1's window objects
  offer no action that presents a window.
* Single-instance mode is the default when launched from the desktop
  ([docs](https://ghostty.org/docs/help/gtk-single-instance)); this machine
  runs `ghostty --gtk-single-instance=true --initial-window=false` and owns
  `com.mitchellh.ghostty`. A Ghostty started with other command-line
  arguments runs a separate instance that does not own the bus name, and its
  surfaces could not be presented this way.

## Design (for Ghostty ≥ 1.4.0)

**Notifications over D-Bus, not `notify-send`.** The daemon talks to
`org.freedesktop.Notifications` directly (one connection, e.g.
`github.com/godbus/dbus/v5`):

* `Notify` with a `default` action, `replaces_id` = the session's previous
  notification (one per session, not a pile), and the session key remembered
  per notification id;
* `ActionInvoked(id, "default")` → a machine input
  `NotificationClicked{Key, At}`; `NotificationClosed` forgets the id;
  `ActivationToken`, if a future GNOME sends it, is kept for the raise.

`notify_command` stays for users who configure their own command; those
notifications are not clickable. `notify-send -A` was considered and rejected:
it is one waiting process per notification, and GNOME keeps notifications in
the tray, so they would live for hours.

**On click** (pure plan in core, executed by the shell):

1. Choose the tmux client: the one that is looking (focus reported, see
   architecture 4.7), else the most recently active client.
2. `core.PlanJump` for the session on that client, as Shift+PgDn does,
   recording the back point (`internal/nav`).
3. Read the client's `GHOSTTY_SURFACE_ID` (from `/proc/<client_pid>/environ`,
   once per client, like the registry's `$TMUX`; the client pid is
   `#{client_pid}`). If present, call
   `org.gtk.Actions.Activate("present-surface", [<uint64 id>], platform_data)`
   on `com.mitchellh.ghostty`, passing the activation token in
   `platform_data["activation-token"]` when there is one.
4. No surface id (another terminal, older Ghostty): step 2 only, and the
   notification says nothing it cannot do.

**Risk to test first:** without an activation token, mutter's focus-stealing
prevention may answer `present-surface` with a "Ghostty is ready" notice
instead of raising the window. Test: with Ghostty 1.4, focus another app,
then from a shell run
`gdbus call --session --dest com.mitchellh.ghostty --object-path /com/mitchellh/ghostty --method org.gtk.Actions.Activate present-surface "[<uint64 $GHOSTTY_SURFACE_ID>]" "{}"`
using a surface id read from a tmux client's environment. If it raises the
window, the design stands. If not, the remaining way is to let Ghostty own
the notification (GNOME activates an application's own notification actions
with a token): Ghostty raises its surface correctly when a program in it
emits an OSC 9 / OSC 777 desktop notification, but then cccp cannot learn of
the click to switch the tmux pane. Decide between the two only after the
test.

## GNOME Terminal (if it is ever used again)

No per-window identity on D-Bus. Options were: let GNOME raise the app's
most recent window (a notification with the `desktop-entry` hint, right only
with one window), or the
[Activate Window By Title](https://github.com/lucaswerkmeister/activate-window-by-title)
extension (GNOME 45–51) with tmux `set-titles` putting a per-client marker in
each window title.
