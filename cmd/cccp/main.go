// Command cccp is the Claude Code control panel.
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
	var err error
	switch cmd {
	case "hook":
		os.Exit(runHook(args, start))
	case "ls", "list":
		err = runList(args)
	case "status":
		err = runStatus(args)
	case "daemon":
		err = runDaemon(args)
	case "notify":
		os.Exit(runNotify(args))
	case "jump":
		err = runJump(args)
	case "ui":
		err = runUI(args)
	case "search":
		err = runSearch(args)
	case "install":
		err = runInstall(args)
	case "uninstall":
		err = runUninstall(args)
	case "doctor":
		err = runDoctor(args)
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
  cccp [ui]              cockpit TUI (run inside tmux; see cccp.tmux for the popup binding)
  cccp ls [--json] [--all]
                         list sessions
  cccp status            one-line counts (as shown in the tmux status line)
  cccp jump [--next] [--client NAME] [KEY]
                         switch a tmux client to a session (--next: most urgent)
  cccp search [--json] QUERY
                         full-text search over transcripts
  cccp daemon            run the daemon (normally via systemd --user)
  cccp install [--yes]   install hooks, tmux integration and the systemd unit
  cccp uninstall         remove them
  cccp doctor            check the installation
  cccp hook claude|agy   hook entry point (called by the agents)
  cccp notify tmux EVENT hook entry point (called by tmux)
`)
}
