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
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"cccp"
	"cccp/internal/catalog"
	"cccp/internal/cgroups"
	"cccp/internal/core"
	"cccp/internal/daemon"
	"cccp/internal/dirs"
	"cccp/internal/hook"
	"cccp/internal/install"
	"cccp/internal/ipc"
	"cccp/internal/machine"
	"cccp/internal/model"
	"cccp/internal/nav"
	"cccp/internal/paths"
	"cccp/internal/present"
	"cccp/internal/tmux"
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
	multi := present.MultiProfile(snap)
	for _, s := range snap.Sessions {
		if s.Group == core.GroupClosed && !all {
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
		badge := present.AgentBadge(s.Agent)
		if multi {
			badge += fmt.Sprintf(" %-8s", present.ProfileTag(s, 8))
		}
		fmt.Fprintf(w, " %s%s %s %-12s %-40s %s\n", present.Glyph(s), inf, badge, clipStr(loc, 12), clipStr(s.DisplayTitle(), 40), meta)
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
	back := fs.Bool("back", false, "return to where you were before cccp's last jump; repeat to toggle")
	client := fs.String("client", "", "tmux client to switch (default: the current one)")
	_ = fs.Parse(args)
	if *client == "" {
		*client = a.currentClient()
	}
	if *back {
		return a.back(*client)
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
		return errors.New("usage: cccp jump --next | cccp jump --back | cccp jump KEY")
	}
	return a.jump(*client, target)
}

// jump executes core.PlanJump for a session on a tmux client, and records
// where the client came from for back.
func (a *app) jump(client string, s core.Session) error {
	from := a.clientValue(client, "#{pane_id}")
	panes, err := a.tmux.ListPanes()
	if err != nil {
		return err
	}
	p := core.PlanJump(s, panes)
	var to string
	switch p.Kind {
	case core.JumpPane:
		if err := a.tmux.SwitchClient(client, p.Pane); err != nil {
			return err
		}
		to = p.Pane
	case core.JumpPlaceholder:
		// The conversation waits in its restored pane: go there and start
		// it (the placeholder resumes on Enter, and refuses if the
		// conversation is open elsewhere).
		if err := a.tmux.SwitchClient(client, p.Pane); err != nil {
			return err
		}
		if err := a.startPlaceholder(p.Pane); err != nil {
			return err
		}
		to = p.Pane
	case core.JumpNewWindow:
		dir := p.Dir
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			if p.Resume {
				return fmt.Errorf("cannot resume %s: its directory %s no longer exists", s.DisplayTitle(), p.Dir)
			}
			dir = ""
		}
		sess := p.TmuxSession
		if sess == "" {
			sess = a.clientValue(client, "#{session_name}")
		}
		pane, err := a.tmux.NewWindow(sess, dir, "", p.Env, p.Argv, false)
		if err != nil {
			return err
		}
		if err := a.tmux.SwitchClient(client, pane); err != nil {
			return err
		}
		to = pane
	default:
		return errors.New(p.Reason)
	}
	if name := a.clientName(client); name != "" {
		opt := nav.Option(name)
		if cur, err := a.tmux.GlobalOption(opt); err == nil {
			_ = a.tmux.SetGlobalOption(opt, nav.Encode(nav.Jumped(nav.Decode(cur), from, to)))
		}
	}
	_ = a.daemon.Send(ipc.Request{Op: ipc.OpSeen, Key: s.Key}, 300*time.Millisecond)
	return nil
}

// back switches the client to where it was before cccp's last jump
// (nav.Return).
func (a *app) back(client string) error {
	name := a.clientName(client)
	if name == "" {
		return errors.New("back: not in tmux (pass --client NAME)")
	}
	opt := nav.Option(name)
	cur, err := a.tmux.GlobalOption(opt)
	if err != nil {
		return err
	}
	target, next, ok := nav.Return(nav.Decode(cur), a.clientValue(name, "#{pane_id}"))
	if !ok {
		return a.tmux.DisplayMessage(name, "cccp: nowhere to go back to")
	}
	if err := a.tmux.SwitchClient(name, target); err != nil {
		_ = a.tmux.SetGlobalOption(opt, "") // the pane is gone: forget it
		return a.tmux.DisplayMessage(name, "cccp: the pane to go back to is gone")
	}
	return a.tmux.SetGlobalOption(opt, nav.Encode(next))
}

// clientName is client, or the current client's name when client is "".
func (a *app) clientName(client string) string {
	if client != "" {
		return client
	}
	return a.currentClient()
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

// ---- tmux-focus-since ------------------------------------------------------------

// runTmuxFocusSince records when the tmux server started asking terminals
// for focus reports (tmux.EnsureFocusSince). cccp.tmux runs it just before
// turning focus-events on.
func runTmuxFocusSince(a *app, args []string) error {
	return a.tmux.EnsureFocusSince(time.Now())
}

// ---- search -------------------------------------------------------------------------

func runSearch(a *app, args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print hits as JSON")
	limit := fs.Int("n", 30, "maximum sessions to list")
	_ = fs.Parse(args)
	q := catalog.ParseQuery(strings.Join(fs.Args(), " "))
	if len(q.Terms) == 0 {
		return errors.New("usage: cccp search [--json] [-n N] TERMS…")
	}
	ix := a.catalog(a.profiles(a.config()))
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
	g := a.gatherer(cfg)
	d := daemon.New(daemon.Options{
		SessionsDir: a.l.SessionsDir,
		Socket:      a.l.Socket,
	}, daemon.Ports{
		Observer:   g,
		Indexer:    indexer{base: a.catalog(nil)},
		Jail:       a.jailer(),
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
	if strings.Contains(*client, "#{") {
		return fmt.Errorf("ui: --client %q is an unexpanded tmux format", *client)
	}
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
		Source:     src,
		Panes:      a.tmux,
		Search:     a.catalog(nil), // only read: the daemon (or localSource) updates it
		Dirs:       dirs.FS{},
		Home:       a.l.Home,
		Workstream: a.clientValue(*client, "#{session_name}"),
		JailStats:  cgroups.New(os.Getuid()).Stats,
		Usage:      a.usageSources,

		Actions: ui.Actions{
			OpenWorkstream: func(name string) error { return a.openWorkstream(*client, name) },
			MoveSession:    func(s core.Session, dest string) error { return a.moveSession(s, dest) },
			SetLimits: func(ws, preset string, d time.Duration) error {
				if !hasDaemon {
					return needDaemon("limits")
				}
				req := ipc.Request{Op: ipc.OpLimits, Key: ws, Value: preset}
				if d > 0 {
					req.For = d.String()
				}
				resp, err := a.daemon.Call(req, 3*time.Second)
				if err == nil && resp.Error != "" {
					err = errors.New(resp.Error)
				}
				return err
			},
			NewWorkstream: func(name, dir string, create bool) error {
				return a.newWorkstream(*client, name, dir, create)
			},
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
			Summarize: func(key string) error {
				if !hasDaemon {
					return nil // summaries are the daemon's: without it there are none
				}
				return a.daemon.Send(ipc.Request{Op: ipc.OpSummarize, Key: key}, time.Second)
			},
		},
	})
}

// runPopup opens the cockpit in a tmux popup on --client. The client must be
// expanded by the caller (the Shift+PgUp binding goes through run-shell,
// which expands #{client_name}): inside a popup tmux can no longer tell
// which client it is on.
func runPopup(a *app, args []string) error {
	fs := flag.NewFlagSet("popup", flag.ExitOnError)
	client := fs.String("client", "", "tmux client to show the popup on (required)")
	_ = fs.Parse(args)
	if *client == "" || strings.Contains(*client, "#{") {
		return errors.New("popup: --client NAME is required (an expanded client name, e.g. from run-shell's #{client_name})")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	var env []string
	for _, k := range paths.EnvVars {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return a.tmux.DisplayPopup(tmux.Popup{
		Client: *client, Width: "92%", Height: "88%", Title: " cccp ", Env: env,
		Argv: []string{self, "ui", "--client", *client},
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
	cfg, mem := l.a.config(), l.a.memory()
	g := l.a.gatherer(cfg)
	start := time.Now()
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		discover := true
		for {
			g.Reload()
			g.RefreshTmux()
			if discover { // at start and on r: as the daemon does
				g.Discover()
				l.a.updateCatalog(g)
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

// execRunner runs external commands for the installer and doctor.
type execRunner struct{}

// Run implements install.Runner: env nil inherits cccp's environment,
// otherwise it is the command's complete environment.
func (execRunner) Run(env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = env // nil: inherit
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// errNoTerminal: the installer would ask, but nothing can answer.
var errNoTerminal = errors.New("no terminal to ask on (e.g. run through `!` or a pipe): run it in a terminal, or pass --yes to do every step")

// isTerminal reports whether f is a terminal (TCGETS succeeds). A character
// device is not enough: /dev/null is one.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

// currentUser is the login name ("" if unknown).
func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func (a *app) installer(yes bool) (*install.Installer, error) {
	if !yes && !isTerminal(os.Stdin) {
		return nil, errNoTerminal
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &install.Installer{
		L: a.l, Profiles: a.profiles(a.config()), Assets: cccp.Assets, Cmd: execRunner{}, Environ: os.Environ(), Tmux: a.tmux, Self: self,
		Out: os.Stdout, In: bufio.NewReader(os.Stdin), Yes: yes, User: currentUser(),
	}, nil
}

func runInstall(a *app, args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	yes := fs.Bool("yes", false, "do every step without asking")
	only := fs.String("only", "", "comma-separated subset: bin,claude,agy,systemd,tmux; host steps, only when named: docker (cap containers' memory, sudo), linger")
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
