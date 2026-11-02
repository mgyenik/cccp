package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"cccp/internal/daemon"
	"cccp/internal/engine"
	"cccp/internal/ipc"
	"cccp/internal/model"
	"cccp/internal/tmux"
)

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	_ = fs.Parse(args)
	logger := log.New(os.Stderr, "", log.LstdFlags)
	return daemon.Run(logger, nil)
}

// runNotify forwards a tmux hook to the daemon. It must be fast and silent:
// tmux runs it on every focus change.
func runNotify(args []string) int {
	if len(args) < 2 || args[0] != "tmux" {
		return 0
	}
	req := ipc.Request{Op: ipc.OpTmux, Event: args[1]}
	if len(args) > 2 {
		req.Pane = args[2]
	}
	if len(args) > 3 {
		req.Client = args[3]
	}
	_ = ipc.Send(req, 300*time.Millisecond)
	return 0
}

func currentClient(tm *tmux.Client) string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	out, err := tm.Run("display-message", "-p", "#{client_name}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func clientPane(tm *tmux.Client, client string) string {
	a := []string{"display-message", "-p"}
	if client != "" {
		a = append(a, "-c", client)
	}
	out, err := tm.Run(append(a, "#{pane_id}")...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func runJump(args []string) error {
	fs := flag.NewFlagSet("jump", flag.ExitOnError)
	next := fs.Bool("next", false, "jump to the most urgent session needing you; repeat to cycle")
	client := fs.String("client", "", "tmux client to switch (default: the current one)")
	_ = fs.Parse(args)
	tm := tmux.Default()
	if *client == "" {
		*client = currentClient(tm)
	}
	snap, err := snapshot(false)
	if err != nil {
		return err
	}
	var target *engine.Session
	switch {
	case *next:
		target = nextAttention(snap, clientPane(tm, *client))
		if target == nil {
			_ = tm.DisplayMessage(*client, "cccp: nothing needs you")
			return nil
		}
	case fs.NArg() == 1:
		for i := range snap.Sessions {
			if snap.Sessions[i].Key == fs.Arg(0) {
				target = &snap.Sessions[i]
			}
		}
		if target == nil {
			return fmt.Errorf("no session %q", fs.Arg(0))
		}
	default:
		return errors.New("usage: cccp jump --next | cccp jump KEY")
	}
	return jumpTo(tm, *client, *target)
}

// nextAttention picks the next session needing attention after the one in
// the current pane, in snapshot order (most urgent first), wrapping around.
func nextAttention(snap engine.Snapshot, curPane string) *engine.Session {
	var cands []int
	for i, s := range snap.Sessions {
		if s.NeedsAttention() && s.Alive && (s.Pane != "" || s.Kind == "background") {
			cands = append(cands, i)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	for j, i := range cands {
		if snap.Sessions[i].Pane == curPane && curPane != "" {
			return &snap.Sessions[cands[(j+1)%len(cands)]]
		}
	}
	return &snap.Sessions[cands[0]]
}

// jumpTo shows a session on a client: its pane if it has one, an attached
// window for a background session, or a resumed session if it ended.
func jumpTo(tm *tmux.Client, client string, s engine.Session) error {
	switch {
	case s.Alive && s.Pane != "":
		if err := tm.SwitchClient(client, s.Pane); err != nil {
			return err
		}
	case s.Alive && s.Kind == "background" && s.SessionID != "":
		pane, err := tm.NewWindow(clientSession(tm, client), s.Cwd, "", []string{"claude", "attach", s.SessionID})
		if err != nil {
			return err
		}
		if err := tm.SwitchClient(client, pane); err != nil {
			return err
		}
	case !s.Alive:
		return resume(tm, client, s)
	default:
		return fmt.Errorf("%s has no tmux pane", s.DisplayTitle())
	}
	_ = ipc.Send(ipc.Request{Op: ipc.OpSeen, Key: s.Key}, 300*time.Millisecond)
	return nil
}

func clientSession(tm *tmux.Client, client string) string {
	a := []string{"display-message", "-p"}
	if client != "" {
		a = append(a, "-c", client)
	}
	out, err := tm.Run(append(a, "#{session_name}")...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// resume opens a new window continuing an ended session.
func resume(tm *tmux.Client, client string, s engine.Session) error {
	if s.SessionID == "" {
		return fmt.Errorf("%s: session id unknown, cannot resume", s.DisplayTitle())
	}
	var argv []string
	switch s.Agent {
	case model.Claude:
		argv = []string{"claude", "--resume", s.SessionID}
	case model.Agy:
		argv = []string{"agy", "--conversation=" + s.SessionID}
	default:
		return fmt.Errorf("cannot resume %s sessions", s.Agent)
	}
	dir := s.Cwd
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = ""
	}
	pane, err := tm.NewWindow(clientSession(tm, client), dir, "", argv)
	if err != nil {
		return err
	}
	return tm.SwitchClient(client, pane)
}
