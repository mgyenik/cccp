package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cccp/internal/core"
	"cccp/internal/dirs"
	"cccp/internal/ipc"
	"cccp/internal/model"
	"cccp/internal/tmux"
	"cccp/internal/ui"
	"cccp/internal/workstream"
)

// Workstreams (docs/workstreams.md): the commands and cockpit actions that
// create, open and restore them. The rules are in internal/workstream; this
// is the shell that runs tmux.

// liveTmux reads the running tmux server for the workstream rules.
func (a *app) liveTmux() (workstream.Live, error) {
	g := a.gatherer(a.config())
	g.RefreshTmux()
	view := g.Tmux()
	live := core.LiveWorkstreams(view, nil)
	if !live.OK {
		return live, fmt.Errorf("tmux: %s", view.Err)
	}
	return live, nil
}

// dormant returns the workstreams to restore: those recorded on a server
// that is gone, whose names are not taken (the same rule the daemon applies:
// one closed on the running server is not dormant).
func (a *app) dormant() ([]workstream.Workstream, workstream.Live, error) {
	rec, err := a.memoryFile().LoadWorkstreams()
	if err != nil {
		return nil, workstream.Live{}, err
	}
	live, err := a.liveTmux()
	if err != nil {
		return nil, live, err
	}
	return workstream.Observe(rec, live, time.Now()).Dormant(), live, nil
}

// restore recreates one workstream, detached.
func (a *app) restore(w workstream.Workstream) error {
	bases, err := a.tmux.Bases()
	if err != nil {
		return err
	}
	if err := a.tmux.Restore(workstream.Plan(w, bases, a.l.BinPath)); err != nil {
		return fmt.Errorf("restoring %s: %w", w.Name, err)
	}
	return nil
}

// running reports whether a tmux session of exactly this name exists.
func (a *app) running(name string) bool {
	_, err := a.tmux.Run("has-session", "-t", "="+name)
	return err == nil
}

// openWorkstream switches the client to a workstream, restoring it first if
// it is dormant.
func (a *app) openWorkstream(client, name string) error {
	if !a.running(name) {
		restored, err := a.restoreDormant(name)
		if err != nil {
			return err
		}
		if !restored {
			return fmt.Errorf("no workstream %q", name)
		}
		a.pokeDaemon()
	}
	return a.tmux.SwitchClient(client, "="+name)
}

// newWorkstream creates a workstream (a tmux session) in dir, making dir
// first if asked, and switches the client to it ("" client: only create).
func (a *app) newWorkstream(client, name, dir string, create bool) error {
	if err := workstream.Validate(name); err != nil {
		return err
	}
	if a.running(name) {
		return fmt.Errorf("workstream %q already exists", name)
	}
	if create {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	} else if !(dirs.FS{}).Exists(dir) {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if _, err := a.tmux.Run("new-session", "-d", "-s", name, "-c", dir); err != nil {
		return err
	}
	a.pokeDaemon()
	if client == "" {
		return nil
	}
	return a.tmux.SwitchClient(client, "="+name)
}

// pokeDaemon asks the daemon to look at tmux now, so the record follows a
// restore or a new workstream without waiting for the next event.
func (a *app) pokeDaemon() {
	_ = a.daemon.Send(ipc.Request{Op: ipc.OpRefresh}, 300*time.Millisecond)
}

// runStart restores the dormant workstreams, then attaches this terminal
// to the most recently used one and opens the cockpit (inside tmux: only
// opens the cockpit). With no workstream at all, it asks for a first one.
func runStart(a *app, args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	_ = fs.Parse(args)
	dormant, live, err := a.dormant()
	if err != nil {
		return err
	}
	restored := 0
	for _, w := range dormant {
		if err := a.restore(w); err != nil {
			a.warn("%v", err)
			continue
		}
		restored++
	}
	if restored > 0 {
		fmt.Printf("cccp: restored %d workstream(s)\n", restored)
		a.pokeDaemon()
	}
	if os.Getenv("TMUX") != "" {
		return runPopup(a, []string{"--client", a.currentClient()})
	}
	target := mostRecent(live, dormant)
	if target == "" {
		c, err := ui.RunCreate(a.snapshot(false), a.l.Home, dirs.FS{})
		if err != nil || c == nil {
			return err // cancelled: nothing to attach to
		}
		target = c.Open
		if target == "" {
			if err := a.newWorkstream("", c.Name, c.Dir, c.Create); err != nil {
				return err
			}
			target = c.Name
		}
	}
	return a.attach(target)
}

// mostRecent is the most recently used workstream: running ones by tmux's
// session activity, restored ones by their recorded last use.
func mostRecent(live workstream.Live, restored []workstream.Workstream) string {
	best, at := "", time.Time{}
	consider := func(name string, t time.Time) {
		if best == "" || t.After(at) {
			best, at = name, t
		}
	}
	for _, p := range live.Panes {
		consider(p.Pane.Session, p.Pane.SessionActivity)
	}
	for _, w := range restored {
		consider(w.Name, w.LastUsed)
	}
	return best
}

// attach replaces this process with tmux attached to the workstream, which
// then opens the cockpit on the attached client (a command chained after
// attach runs as that client, so #{client_name} is the new one).
func (a *app) attach(name string) error {
	path, err := exec.LookPath("tmux")
	if err != nil {
		return err
	}
	argv := []string{"tmux"}
	if a.l.TmuxSocket != "" {
		argv = append(argv, "-S", a.l.TmuxSocket)
	}
	popup := tmux.ShellQuote(a.l.BinPath) + " popup --client '#{client_name}'"
	argv = append(argv, "attach-session", "-t", "="+name, ";", "run-shell", "-b", popup)
	return syscall.Exec(path, argv, os.Environ())
}

// runResumePane is a restored conversation's placeholder: it waits for
// Enter, then resumes the conversation (its profile selected, in this
// directory) as a child, and leaves a shell in the pane when it ends.
// Ctrl-D or Ctrl-C gives the shell straight away.
func runResumePane(a *app, args []string) error {
	fs := flag.NewFlagSet("resume-pane", flag.ExitOnError)
	agent := fs.String("agent", "claude", "claude or agy")
	id := fs.String("id", "", "the conversation's session id")
	configDir := fs.String("config-dir", "", "the Claude profile's directory")
	def := fs.Bool("default-profile", false, "the profile is the default one (CLAUDE_CONFIG_DIR unset)")
	title := fs.String("title", "", "shown while waiting")
	_ = fs.Parse(args)
	cwd, _ := os.Getwd()
	s := core.Session{Agent: model.Agent(*agent), SessionID: *id, Cwd: cwd, ConfigDir: *configDir,
		DefaultProfile: *def, Title: *title}
	plan := core.PlanJump(s, nil)
	if plan.Kind != core.JumpNewWindow {
		fmt.Printf("\n  cannot resume: %s\n\n", plan.Reason)
		return a.paneShell(cwd)
	}
	name := *title
	if name == "" {
		name = *id
	}
	fmt.Printf("\n  ⏎  resume «%s»\n\n     %s\n     in %s\n\n  Enter resumes it · Ctrl-D or Ctrl-C: a shell here instead\n",
		name, strings.Join(plan.Argv, " "), cwd)
	if !waitForEnter() {
		return a.paneShell(cwd)
	}
	cmd := exec.Command(plan.Argv[0], plan.Argv[1:]...)
	cmd.Env = append(os.Environ(), plan.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The terminal's Ctrl-C and Ctrl-\ are the conversation's, not ours.
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT)
	err := cmd.Run()
	signal.Reset(syscall.SIGINT, syscall.SIGQUIT)
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			fmt.Printf("\n  could not start %s: %v\n", plan.Argv[0], err)
		}
	}
	return a.paneShell(cwd)
}

// waitForEnter reads a line; false on EOF (Ctrl-D) or Ctrl-C.
func waitForEnter() bool {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT)
	defer signal.Stop(sig)
	line := make(chan bool, 1)
	go func() {
		_, err := bufio.NewReader(os.Stdin).ReadString('\n')
		line <- err == nil
	}()
	select {
	case ok := <-line:
		return ok
	case <-sig:
		return false
	}
}

// paneShell clears the placeholder mark (the pane is a shell from now on)
// and replaces this process with a login shell, as tmux starts one.
func (a *app) paneShell(cwd string) error {
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		_, _ = a.tmux.Run("set-option", "-p", "-u", "-t", pane, workstream.ResumeOption)
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	if cwd != "" {
		_ = os.Chdir(cwd)
	}
	return syscall.Exec(shell, []string{"-" + filepath.Base(shell)}, os.Environ())
}

// moveSession moves a session into the workstream dest, which is created
// (in the session's directory) or restored first if need be. The client
// stays where it is (docs/workstreams.md).
func (a *app) moveSession(s core.Session, dest string) error {
	if err := workstream.Validate(dest); err != nil {
		return err
	}
	p := core.PlanMove(s, dest)
	if p.Kind == core.MoveImpossible {
		return errors.New(p.Reason)
	}
	if p.Kind == core.MoveOpen && p.Open.Resume && !(dirs.FS{}).Exists(p.Open.Dir) {
		return fmt.Errorf("cannot resume %s: its directory %s no longer exists", s.DisplayTitle(), p.Open.Dir)
	}
	if !a.running(dest) {
		restored, err := a.restoreDormant(dest)
		if err != nil {
			return err
		}
		if !restored {
			if err := a.moveIntoNew(s, p, dest); err != nil {
				return err
			}
			a.pokeDaemon()
			return nil
		}
	}
	switch p.Kind {
	case core.MoveWindow:
		if _, err := a.tmux.Run("move-window", "-s", p.Window, "-t", "="+dest+":"); err != nil {
			return err
		}
	case core.MoveOpen:
		if _, err := a.tmux.NewWindow(dest, p.Open.Dir, "", p.Open.Env, p.Open.Argv, true); err != nil {
			return err
		}
	}
	a.pokeDaemon()
	return nil
}

// moveIntoNew makes the workstream dest out of the session being moved.
//
// A window to open becomes the new session's first window: new-session runs
// its command. A running window cannot become a session itself (new-session
// always makes its own first window), so the session is made with a
// placeholder and move-window -k puts the window in its place, killing it.
// The placeholder runs sleep, never the user's shell: a shell killed while
// it starts up can leave its startup half done (pyenv's rehash lock was
// left behind that way, and every later shell waited 60 s for it).
func (a *app) moveIntoNew(s core.Session, p core.Move, dest string) error {
	dir := s.Cwd // also the directory a window to open runs in (core.PlanJump)
	if !(dirs.FS{}).Exists(dir) {
		dir = a.l.Home // attaching a background session works from anywhere
	}
	if p.Kind == core.MoveOpen {
		args := []string{"new-session", "-d", "-s", dest, "-c", dir}
		for _, kv := range p.Open.Env {
			args = append(args, "-e", kv)
		}
		_, err := a.tmux.Run(append(args, p.Open.Argv...)...)
		return err
	}
	out, err := a.tmux.Run("new-session", "-d", "-P", "-F", "#{window_index}", "-s", dest, "-c", dir, "sleep", "86400")
	if err != nil {
		return err
	}
	_, err = a.tmux.Run("move-window", "-k", "-s", p.Window, "-t", "="+dest+":"+strings.TrimSpace(out))
	return err
}

// restoreDormant restores the dormant workstream name, if there is one.
func (a *app) restoreDormant(name string) (bool, error) {
	dormant, _, err := a.dormant()
	if err != nil {
		return false, err
	}
	for _, w := range dormant {
		if w.Name == name {
			return true, a.restore(w)
		}
	}
	return false, nil
}
