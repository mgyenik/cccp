// Command cccp is the control panel for Claude Code and Antigravity CLI
// sessions in tmux.
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	start := time.Now() // first thing: hook event timestamps
	args := os.Args[1:]
	cmd := "ui"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	// The hook path stays minimal: it must never fail or print.
	if cmd == "hook" {
		os.Exit(runHook(newApp(), args, start))
	}
	if cmd == "notify" {
		os.Exit(runNotify(newApp(), args))
	}
	a := newApp()
	var err error
	switch cmd {
	case "ls", "list":
		err = runList(a, args)
	case "status":
		err = runStatus(a, args)
	case "daemon":
		err = runDaemon(a, args)
	case "jump":
		err = runJump(a, args)
	case "ui":
		err = runUI(a, args)
	case "popup":
		err = runPopup(a, args)
	case "search":
		err = runSearch(a, args)
	case "install":
		err = runInstall(a, args)
	case "uninstall":
		err = runUninstall(a, args)
	case "doctor":
		err = runDoctor(a, args)
	case "tmux-focus-since":
		err = runTmuxFocusSince(a, args)
	case "start":
		err = runStart(a, args)
	case "resume-pane":
		err = runResumePane(a, args)
	case "limits":
		err = runLimits(a, args)

	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "cccp: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cccp:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `cccp — control panel for Claude Code and Antigravity CLI sessions in tmux

usage:
  cccp [ui] [--client NAME]
                         cockpit TUI (Shift+PgUp in tmux, via cccp.tmux)
  cccp popup --client NAME
                         open the cockpit in a tmux popup on a client
  cccp ls [--json] [--all] [--local]
                         list sessions
  cccp status [--tmux]   one-line counts (as in the tmux status line)
    cccp jump [--next|--back] [--client NAME] [KEY]
                         switch a tmux client to a session (--next: most urgent;
                         --back: to where it was before cccp's last jump)
  cccp search [--json] [-n N] TERMS…
                         full-text search over transcripts
  cccp daemon            run the daemon (normally via systemd --user)
  cccp install [--yes] [--only bin,claude,agy,systemd,tmux]
                         install hooks, tmux integration and the systemd unit
  cccp uninstall [--yes] remove them
  cccp doctor            check the installation
  cccp hook claude|agy   hook entry point (called by the agents)
    cccp notify tmux EVENT tmux hook entry point (called by tmux)
    cccp tmux-focus-since  record when focus reporting started (called by cccp.tmux)
    cccp start             restore the workstreams (after a reboot), attach, open the cockpit
  cccp limits [WORKSTREAM normal|boost|max [--for 2h|--until-undone]]
                         show the workstreams' memory and CPU limits, or boost one

  cccp resume-pane       a restored conversation's placeholder (run by cccp start)
`)
}
