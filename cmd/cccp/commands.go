package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"cccp"
	"cccp/internal/core"
	"cccp/internal/daemon"
	"cccp/internal/hook"
	"cccp/internal/install"
	"cccp/internal/ipc"
	"cccp/internal/machine"
	"cccp/internal/model"
	"cccp/internal/present"
	"cccp/internal/search"
	"cccp/internal/ui"
)

// ---- hook / notify: called by the agents and by tmux -------------------------

// runHook is the agents' hook entry point. Whatever happens it exits 0 and
// writes nothing a hook runner could read as a decision: for agy, which
// requires JSON on stdout, it prints "{}"; for Claude it prints nothing.
func runHook(a *app, args []string, start time.Time) int {
	agent := model.Agent("")
	if len(args) > 0 {
		agent = model.Agent(args[0])
	}
	defer func() {
		if r := recover(); r != nil {
			hookLog(a, fmt.Errorf("panic: %v", r))
		}
		if agent == model.Agy {
			fmt.Println("{}")
		}
	}()
	deps := hook.Deps{Getenv: os.Getenv, FindAgent: hook.SystemFindAgent(os.Getppid()), Store: a.store}
	if err := hook.Handle(deps, agent, os.Stdin, start); err != nil {
		hookLog(a, err)
	}
	return 0
}

// hookLog appends a hook error to the log file (never to the agent's output).
func hookLog(a *app, err error) {
	_ = os.MkdirAll(filepath.Dir(a.l.HookErrorLog), 0o700)
	f, ferr := os.OpenFile(a.l.HookErrorLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %v\n", time.Now().Format(time.RFC3339), err)
}

// runNotify forwards a tmux hook to the daemon. It must be fast and silent:
// tmux runs it on every focus change.
func runNotify(a *app, args []string) int {
	if len(args) < 2 || args[0] != "tmux" {
		return 0
	}
	_ = a.daemon.Send(ipc.Request{Op: ipc.OpTmux, Event: args[1]}, 300*time.Millisecond)
	return 0
}

// ---- ls / status ----------------------------------------------------------------

func runList(a *app, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the snapshot as JSON")
	all := fs.Bool("all", false, "include ended sessions")
	local := fs.Bool("local", false, "compute in-process even if the daemon is running")
	_ = fs.Parse(args)
	var snap core.Snapshot
	if *local {
		snap = a.localSnapshot()
	} else {
		snap = a.snapshot(false)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(snap)
	}
	printList(os.Stdout, snap, *all)
	return nil
}

func printList(w io.Writer, snap core.Snapshot, all bool) {
	now := snap.At
	src := "in-process"
	if snap.FromDaemon {
		src = "daemon"
	}
	if snap.OnBattery {
		src += ", on battery"
	}
	fmt.Fprintf(w, "%s   (%s)\n", present.PlainStatus(snap), src)
	for _, p := range snap.Problems {
		fmt.Fprintf(w, "! %s\n", p)
	}
	last := core.Group(-1)
	for _, s := range snap.Sessions {
		if s.Group == core.GroupEnded && !all {
			continue
		}
		if s.Group != last {
			fmt.Fprintf(w, "\n%s\n", s.Group)
			last = s.Group
		}
		inf := " "
		if s.Inferred {
			inf = "~"
		}
		loc := s.Location
		if loc == "" {
			loc = "-"
		}
		meta := strings.Join(nonEmpty(present.Since(now, s.ActivitySince), present.CacheText(s, now), present.Tokens(s.ContextTokens)), "  ")
		fmt.Fprintf(w, " %s%s %s %-12s %-40s %s\n", present.Glyph(s), inf, present.AgentBadge(s.Agent), clipStr(loc, 12), clipStr(s.DisplayTitle(), 40), meta)
		if d := present.Detail(s); d != "" {
			fmt.Fprintf(w, "        %s\n", clipStr(d, 110))
		}
	}
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func clipStr(s string, n int) string {
	r := []rune(present.OneLine(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func runStatus(a *app, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	tmuxStyle := fs.Bool("tmux", false, "include tmux style sequences")
	_ = fs.Parse(args)
	snap := a.snapshot(false)
	if *tmuxStyle {
		fmt.Println(present.StatusSegment(snap))
	} else {
		fmt.Println(present.PlainStatus(snap))
	}
	return nil
}

// ---- jump -------------------------------------------------------------------------

func runJump(a *app, args []string) error {
	fs := flag.NewFlagSet("jump", flag.ExitOnError)
	next := fs.Bool("next", false, "jump to the most urgent session needing you; repeat to cycle")
	client := fs.String("client", "", "tmux client to switch (default: the current one)")
	_ = fs.Parse(args)
	if *client == "" {
		*client = a.currentClient()
	}
	snap := a.snapshot(false)
	var target core.Session
	switch {
	case *next:
		s, ok := core.NextAttention(snap, a.clientValue(*client, "#{pane_id}"))
		if !ok {
			return a.tmux.DisplayMessage(*client, "cccp: nothing needs you")
		}
		target = s
	case fs.NArg() == 1:
		found := false
		for _, s := range snap.Sessions {
			if s.Key == fs.Arg(0) {
				target, found = s, true
			}
		}
		if !found {
			return fmt.Errorf("no session %q", fs.Arg(0))
		}
	default:
		return errors.New("usage: cccp jump --next | cccp jump KEY")
	}
	return a.jump(*client, target)
}

// jump executes core.PlanJump for a session on a tmux client.
func (a *app) jump(client string, s core.Session) error {
	p := core.PlanJump(s)
	switch p.Kind {
	case core.JumpPane:
		if err := a.tmux.SwitchClient(client, p.Pane); err != nil {
			return err
		}
	case core.JumpNewWindow:
		dir := p.Dir
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			dir = ""
		}
		pane, err := a.tmux.NewWindow(a.clientValue(client, "#{session_name}"), dir, "", p.Argv)
		if err != nil {
			return err
		}
		if err := a.tmux.SwitchClient(client, pane); err != nil {
			return err
		}
	default:
		return errors.New(p.Reason)
	}
	_ = a.daemon.Send(ipc.Request{Op: ipc.OpSeen, Key: s.Key}, 300*time.Millisecond)
	return nil
}

func (a *app) currentClient() string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	return a.clientValue("", "#{client_name}")
}

// clientValue expands a format for a tmux client ("" = the current one).
func (a *app) clientValue(client, format string) string {
	args := []string{"display-message", "-p"}
	if client != "" {
		args = append(args, "-c", client)
	}
	out, err := a.tmux.Run(append(args, format)...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ---- search -------------------------------------------------------------------------

func runSearch(a *app, args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print hits as JSON")
	limit := fs.Int("n", 30, "maximum sessions to list")
	_ = fs.Parse(args)
	q := search.ParseQuery(strings.Join(fs.Args(), " "))
	if len(q.Terms) == 0 {
		return errors.New("usage: cccp search [--json] [-n N] TERMS…")
	}
	ix := a.searchIndex()
	if _, err := ix.Update(); err != nil {
		return err
	}
	hits, err := ix.Run(q, *limit)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(hits)
	}
	now := time.Now()
	for _, h := range hits {
		fmt.Printf("%s %-5s %-44s %s  (%d)\n", present.AgentBadge(h.Doc.Agent), present.Since(now, h.At), clipStr(h.Doc.Title, 44), h.Doc.Cwd, h.Matches)
		fmt.Printf("      %s: %s\n", h.Role, clipStr(h.Snippet, 140))
		fmt.Printf("      %s\n", h.Doc.Key())
	}
	if len(hits) == 0 {
		fmt.Println("no matches")
	}
	return nil
}

// ---- daemon -------------------------------------------------------------------------

func runDaemon(a *app, args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	_ = fs.Parse(args)
	logger := log.New(os.Stderr, "", log.LstdFlags)
	cfg, err := a.loadConfig()
	if err != nil {
		logger.Printf("config: %v (using defaults)", err)
	}
	mem, err := a.memoryFile().Load()
	if err != nil {
		logger.Printf("memory: %v (starting fresh)", err)
	}
	m := machine.New(cfg, mem, time.Now())
	d := daemon.New(daemon.Options{
		SessionsDir: a.l.SessionsDir,
		RegistryDir: a.l.ClaudeRegistryDir,
		Socket:      a.l.Socket,
	}, daemon.Ports{
		Observer:   a.gatherer(),
		Tmux:       daemon.TmuxOptions{Client: a.tmux},
		Notifier:   daemon.ExecNotifier{},
		Summarizer: daemon.ClaudeSummarizer{Dir: a.l.CacheDir},
		Memory:     a.memoryFile(),
		Files:      a.store,
		LoadConfig: a.loadConfig,
		Now:        time.Now,
		Log:        logger,
	}, m)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	reload := make(chan struct{}, 1)
	go func() {
		for range hup {
			select {
			case reload <- struct{}{}:
			default:
			}
		}
	}()
	return d.Run(ctx, reload)
}

// ---- cockpit --------------------------------------------------------------------------

func runUI(a *app, args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	client := fs.String("client", "", "tmux client to switch when jumping (default: current)")
	_ = fs.Parse(args)
	if *client == "" {
		*client = a.currentClient()
	}
	var src ui.Source
	if sub, err := a.daemon.Subscribe(time.Second); err == nil {
		src = &daemonSource{sub: sub, client: a.daemon}
	} else {
		src = &localSource{a: a}
	}
	hasDaemon := src.Daemon()
	needDaemon := func(what string) error {
		return fmt.Errorf("%s need the daemon (systemctl --user start cccp)", what)
	}
	return ui.Run(ui.Options{
		Source: src,
		Panes:  a.tmux,
		Search: a.searchIndex(),
		Actions: ui.Actions{
			Jump: func(s core.Session) error { return a.jump(*client, s) },
			Alias: func(key, value string) error {
				if !hasDaemon {
					return needDaemon("aliases")
				}
				_, err := a.daemon.Call(ipc.Request{Op: ipc.OpAlias, Key: key, Value: value}, 2*time.Second)
				return err
			},
			Seen: func(key string) error {
				if !hasDaemon {
					return needDaemon("seen marks")
				}
				_, err := a.daemon.Call(ipc.Request{Op: ipc.OpSeen, Key: key}, 2*time.Second)
				return err
			},
		},
	})
}

// daemonSource streams snapshots from the daemon.
type daemonSource struct {
	sub    *ipc.Subscription
	client ipc.Client
}

func (d *daemonSource) Start(send func(core.Snapshot)) {
	go func() {
		for {
			s, err := d.sub.Next()
			if err != nil {
				return
			}
			send(s)
		}
	}()
}

func (d *daemonSource) Refresh() {
	go func() { _, _ = d.client.Call(ipc.Request{Op: ipc.OpRefresh}, 5*time.Second) }()
}

func (d *daemonSource) Daemon() bool { return true }
func (d *daemonSource) Close()       { d.sub.Close() }

// localSource computes snapshots in-process while the cockpit is open.
// Without a daemon there are no change events, so it recomputes every two
// seconds; this happens only while the cockpit is on screen.
type localSource struct {
	a    *app
	once sync.Once
	stop chan struct{}
	kick chan struct{}
}

func (l *localSource) Start(send func(core.Snapshot)) {
	l.stop, l.kick = make(chan struct{}), make(chan struct{}, 1)
	g := l.a.gatherer()
	cfg, mem := l.a.config(), l.a.memory()
	start := time.Now()
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		discover := true
		for {
			g.Reload()
			g.RefreshTmux()
			if discover {
				g.Discover()
			}
			now := time.Now()
			snap, next := core.Compute(g.Observe(now), mem, cfg, start, now)
			mem = next // in-memory only: the daemon owns persisted memory
			send(snap)
			select {
			case <-l.stop:
				return
			case <-t.C:
				discover = false
			case <-l.kick:
				discover = true
			}
		}
	}()
}

func (l *localSource) Refresh() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

func (l *localSource) Daemon() bool { return false }
func (l *localSource) Close()       { l.once.Do(func() { close(l.stop) }) }

// ---- install / uninstall ---------------------------------------------------------------

// execRunner runs external commands for the installer.
type execRunner struct{}

func (execRunner) Run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func (a *app) installer(yes bool) (*install.Installer, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &install.Installer{
		L: a.l, Assets: cccp.Assets, Cmd: execRunner{}, Tmux: a.tmux, Self: self,
		Out: os.Stdout, In: bufio.NewReader(os.Stdin), Yes: yes,
	}, nil
}

func runInstall(a *app, args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	yes := fs.Bool("yes", false, "do every step without asking")
	only := fs.String("only", "", "comma-separated subset: bin,claude,agy,systemd,tmux")
	_ = fs.Parse(args)
	want := map[string]bool{}
	for _, s := range strings.Split(*only, ",") {
		if s = strings.TrimSpace(s); s != "" {
			want[s] = true
		}
	}
	in, err := a.installer(*yes)
	if err != nil {
		return err
	}
	return in.Install(want)
}

func runUninstall(a *app, args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	yes := fs.Bool("yes", false, "do every step without asking")
	_ = fs.Parse(args)
	in, err := a.installer(*yes)
	if err != nil {
		return err
	}
	return in.Uninstall()
}
