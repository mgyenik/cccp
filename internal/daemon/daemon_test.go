package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cccp/internal/config"
	"cccp/internal/core"
	"cccp/internal/ipc"
	"cccp/internal/machine"
	"cccp/internal/model"
	"cccp/internal/tmux"
)

// --- fakes (all guarded by one mutex; the daemon calls them from its loop) ---

type fakes struct {
	mu sync.Mutex

	// observer: like real hook state, the activity's timestamp changes only
	// when the activity does (set through setActivity).
	activity  model.Activity
	since     time.Time
	question  string
	message   string
	reloads   int
	tmuxReads int
	discovers int
	observes  int
	hookFiles []string
	regFiles  []string
	rediscov  bool
	ended     bool
	profiles  []model.Profile

	catalogReads int
	indexRuns    int
	indexHold    chan struct{}
	closedConv   bool // observe a closed conversation "claude:old"

	// effects
	status    []string
	glyphs    []string
	notified  [][]string
	summaries []string
	saved     []core.Memory
	removed   []string
	tmuxErr   error
	logs      []string
}

func (f *fakes) Reload()      { f.mu.Lock(); f.reloads++; f.mu.Unlock() }
func (f *fakes) RefreshTmux() { f.mu.Lock(); f.tmuxReads++; f.mu.Unlock() }
func (f *fakes) Discover()    { f.mu.Lock(); f.discovers++; f.mu.Unlock() }

func (f *fakes) HookFileChanged(p string) core.HookChange {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hookFiles = append(f.hookFiles, p)
	return core.HookChange{Rediscover: f.rediscov, Ended: f.ended}
}

func (f *fakes) RefreshCatalog() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.catalogReads++
}

// Update is the fake Indexer: it records runs and can be held open.
func (f *fakes) Update(dirs []string) error {
	f.mu.Lock()
	f.indexRuns++
	hold := f.indexHold
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	return nil
}

func (f *fakes) RegistryFileChanged(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regFiles = append(f.regFiles, p)
	return false
}

func (f *fakes) Profiles() []model.Profile {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.Profile(nil), f.profiles...)
}

func (f *fakes) Observe(now time.Time) core.Observation {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observes++
	st := model.HookState{Version: 1, Agent: model.Claude, SessionID: "s1", PID: 42, PIDStart: 1, Pane: "%1",
		Activity: f.activity, ActivityAt: f.since, Question: f.question,
		LastMessage: f.message, StartedAt: time.Unix(0, 0)}
	var convs []core.Conversation
	if f.closedConv {
		convs = []core.Conversation{{Agent: model.Claude, SessionID: "old", Cwd: "/w", Title: "Old one",
			FirstPrompt: "do the thing", LastMessage: "Did it.", Last: time.Unix(1, 0)}}
	}
	return core.Observation{
		Conversations: convs,
		Profiles:      append([]model.Profile(nil), f.profiles...),
		Hooks:         []core.HookRecord{{Path: "/state/s1.json", State: st, ProcAlive: true}},
		Tmux: core.TmuxView{OK: true, Panes: []tmux.Pane{{ID: "%1", Session: "s", WindowID: "@1", WindowIndex: 1, WindowPanes: 1}},
			Clients: []tmux.ClientInfo{{Name: "c", Pane: "%9", Focused: true}}},
	}
}

func (f *fakes) SetStatus(v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = append(f.status, v)
	return f.tmuxErr
}

func (f *fakes) SetGlyph(w, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.glyphs = append(f.glyphs, w+"="+v)
	return f.tmuxErr
}

func (f *fakes) Notify(argv []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified = append(f.notified, argv)
	return nil
}

func (f *fakes) Summarize(ctx context.Context, modelName string, prof model.Profile, systemPrompt, input string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summaries = append(f.summaries, prof.Dir+": "+input)
	return "Approve make install", nil
}

func (f *fakes) Save(m core.Memory) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, m)
	return nil
}

func (f *fakes) Remove(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, p)
	return nil
}

func (f *fakes) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, string(p))
	return len(p), nil
}

// setActivity changes the observed activity, as a hook event would.
func (f *fakes) setActivity(a model.Activity, question, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activity, f.question, f.message = a, question, message
	f.since = time.Now()
}

func (f *fakes) set(fn func(*fakes)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakes) get(fn func(*fakes) bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn(f)
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type harness struct {
	f      *fakes
	dir    string
	client ipc.Client
	cancel context.CancelFunc
	done   chan error
	reload chan struct{}
}

func start(t *testing.T, cfg config.Config) *harness {
	t.Helper()
	dir := t.TempDir()
	f := &fakes{activity: model.Working, since: time.Unix(1, 0)}
	opt := Options{
		SessionsDir: filepath.Join(dir, "sessions"),
		Socket:      filepath.Join(dir, "d.sock"),
	}
	f.profiles = []model.Profile{{Name: "default", Dir: filepath.Join(dir, "claude"), Default: true}}
	if err := os.MkdirAll(f.profiles[0].RegistryDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	ports := Ports{
		Observer: f, Tmux: f, Notifier: f, Summarizer: f, Memory: f, Files: f, Indexer: f,
		LoadConfig: func() (config.Config, error) { return cfg, nil },
		Now:        time.Now,
		Log:        log.New(f, "", 0),
	}
	mem := core.NewMemory()
	mem.Initialized = true
	d := New(opt, ports, machine.New(cfg, mem, time.Unix(0, 0)))
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{f: f, dir: dir, client: ipc.Client{Socket: opt.Socket}, cancel: cancel, done: make(chan error, 1), reload: make(chan struct{}, 1)}
	go func() { h.done <- d.Run(ctx, h.reload) }()
	eventually(t, "daemon socket", func() bool {
		_, err := h.client.Call(ipc.Request{Op: ipc.OpSnapshot}, time.Second)
		return err == nil
	})
	t.Cleanup(func() { h.stop(t) })
	return h
}

func (h *harness) stop(t *testing.T) {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("daemon did not stop")
	}
}

func testCfg() config.Config {
	c := config.Default()
	c.NotifyCommand = []string{"notify"}
	c.Summaries = "off"
	return c
}

// --- tests ---------------------------------------------------------------------

func TestStartupObservesEverythingAndServesSnapshots(t *testing.T) {
	h := start(t, testCfg())
	if !h.f.get(func(f *fakes) bool { return f.reloads == 1 && f.tmuxReads == 1 && f.discovers == 1 && f.observes >= 1 }) {
		t.Fatalf("startup scope: %+v", h.f)
	}
	resp, err := h.client.Call(ipc.Request{Op: ipc.OpSnapshot}, time.Second)
	if err != nil || resp.Snapshot == nil || len(resp.Snapshot.Sessions) != 1 || !resp.Snapshot.FromDaemon {
		t.Fatalf("%v %+v", err, resp)
	}
	sub, err := h.client.Subscribe(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	if snap, err := sub.Next(); err != nil || len(snap.Sessions) != 1 {
		t.Fatalf("subscription: %v %+v", err, snap)
	}
	if !h.f.get(func(f *fakes) bool {
		return len(f.status) == 1 && strings.Contains(f.status[0], "●1") && len(f.glyphs) == 1
	}) {
		t.Fatalf("tmux decoration: %v %v", h.f.status, h.f.glyphs)
	}
}

func TestHookFileChangeIsObservedAndNotifies(t *testing.T) {
	h := start(t, testCfg())
	sub, _ := h.client.Subscribe(time.Second)
	defer sub.Close()
	sub.Next()
	before := 0
	h.f.get(func(f *fakes) bool { before = f.observes; return true })

	// Temp and lock files are ignored.
	os.WriteFile(filepath.Join(h.dir, "sessions", ".claude-s1.json.123.tmp"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(h.dir, "sessions", ".lock"), nil, 0o600)

	h.f.setActivity(model.NeedsApproval, "Allow Bash: make install", "")
	p := filepath.Join(h.dir, "sessions", "claude-s1.json")
	os.WriteFile(p, []byte("{}"), 0o600)
	snap, err := sub.Next()
	if err != nil || snap.Sessions[0].Group != core.GroupNeedsYou {
		t.Fatalf("%v %+v", err, snap)
	}
	if !h.f.get(func(f *fakes) bool {
		for _, x := range f.hookFiles {
			if x != p {
				return false // a temp or lock file reached the observer
			}
		}
		return len(f.hookFiles) >= 1 && f.observes > before
	}) {
		t.Fatalf("hook files %v", h.f.hookFiles)
	}
	eventually(t, "notification", func() bool {
		return h.f.get(func(f *fakes) bool {
			return len(f.notified) == 1 && f.notified[0][0] == "notify" && strings.Contains(f.notified[0][2], "Allow Bash: make install")
		})
	})
}

func TestTmuxEventScopes(t *testing.T) {
	for event, want := range map[string]Scope{
		"session-window-changed": scopeTmux,
		"client-focus-in":        scopeTmux,
		"pane-exited":            scopeDiscover,
		"after-new-window":       scopeDiscover,
	} {
		if got := TmuxEventScope(event); got != want {
			t.Errorf("%s: %+v want %+v", event, got, want)
		}
	}
	h := start(t, testCfg())
	var tmux0, disc0 int
	h.f.get(func(f *fakes) bool { tmux0, disc0 = f.tmuxReads, f.discovers; return true })
	h.client.Send(ipc.Request{Op: ipc.OpTmux, Event: "client-focus-in"}, time.Second)
	eventually(t, "tmux reread", func() bool { return h.f.get(func(f *fakes) bool { return f.tmuxReads == tmux0+1 }) })
	if h.f.get(func(f *fakes) bool { return f.discovers != disc0 }) {
		t.Fatal("a focus change rescanned processes")
	}
	h.client.Send(ipc.Request{Op: ipc.OpTmux, Event: "pane-exited"}, time.Second)
	eventually(t, "rediscovery", func() bool { return h.f.get(func(f *fakes) bool { return f.discovers == disc0+1 }) })
}

func TestSeenAliasAndRefreshRequests(t *testing.T) {
	h := start(t, testCfg())
	if _, err := h.client.Call(ipc.Request{Op: ipc.OpAlias, Key: "claude:s1", Value: "box"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Call(ipc.Request{Op: ipc.OpSeen, Key: "claude:s1"}, time.Second); err != nil {
		t.Fatal(err)
	}
	resp, err := h.client.Call(ipc.Request{Op: ipc.OpRefresh}, time.Second)
	if err != nil || resp.Snapshot.Sessions[0].Alias != "box" {
		t.Fatalf("%v %+v", err, resp)
	}
	if !h.f.get(func(f *fakes) bool { return f.reloads == 2 && f.discovers == 2 }) {
		t.Fatalf("refresh scope: reloads=%d discovers=%d", h.f.reloads, h.f.discovers)
	}
	if _, err := h.client.Call(ipc.Request{Op: "bogus"}, time.Second); err == nil {
		t.Fatal("unknown op accepted")
	}
}

func TestSummariesRunThroughThePort(t *testing.T) {
	cfg := testCfg()
	cfg.Summaries = "always"
	h := start(t, cfg)
	h.f.setActivity(model.NeedsApproval, "Allow Bash: make install", "Ready.")
	h.client.Call(ipc.Request{Op: ipc.OpRefresh}, time.Second)
	eventually(t, "summary shown", func() bool {
		resp, err := h.client.Call(ipc.Request{Op: ipc.OpSnapshot}, time.Second)
		return err == nil && resp.Snapshot.Sessions[0].Summary == "Approve make install"
	})
	if !h.f.get(func(f *fakes) bool {
		return len(f.summaries) == 1 && strings.Contains(f.summaries[0], "make install") &&
			strings.HasPrefix(f.summaries[0], f.profiles[0].Dir+": ")
	}) {
		t.Fatalf("summaries %v", h.f.summaries)
	}
}

func TestStopSavesAndClearsTmux(t *testing.T) {
	h := start(t, testCfg())
	h.stop(t)
	if !h.f.get(func(f *fakes) bool {
		return len(f.saved) >= 1 && f.status[len(f.status)-1] == "" && f.glyphs[len(f.glyphs)-1] == "@1="
	}) {
		t.Fatalf("saved=%d status=%q glyphs=%q", len(h.f.saved), h.f.status, h.f.glyphs)
	}
}

func TestFailingTmuxWritesAreResent(t *testing.T) {
	h := start(t, testCfg())
	h.f.set(func(f *fakes) { f.tmuxErr = errors.New("server gone") })
	h.f.setActivity(model.NeedsApproval, "", "")
	h.client.Call(ipc.Request{Op: ipc.OpRefresh}, time.Second)
	var n int
	h.f.get(func(f *fakes) bool { n = len(f.status); return true })
	h.f.set(func(f *fakes) { f.tmuxErr = nil })
	// Same state: normally nothing would be re-sent, but the failure forces it.
	h.client.Call(ipc.Request{Op: ipc.OpRefresh}, time.Second)
	if !h.f.get(func(f *fakes) bool { return len(f.status) == n+1 }) {
		t.Fatalf("status writes: before %d after %d", n, len(h.f.status))
	}
	if !h.f.get(func(f *fakes) bool {
		for _, l := range f.logs {
			if strings.Contains(l, "tmux") {
				return true
			}
		}
		return false
	}) {
		t.Fatal("tmux failure not logged")
	}
}

func TestConfigReload(t *testing.T) {
	h := start(t, testCfg())
	h.reload <- struct{}{}
	eventually(t, "reload logged", func() bool {
		return h.f.get(func(f *fakes) bool {
			for _, l := range f.logs {
				if strings.Contains(l, "config reloaded") {
					return true
				}
			}
			return false
		})
	})
}

func TestSecondDaemonRefusesToStart(t *testing.T) {
	h := start(t, testCfg())
	d := New(Options{SessionsDir: filepath.Join(h.dir, "s2"), Socket: h.client.Socket},
		Ports{Log: log.New(io.Discard, "", 0), Now: time.Now}, machine.New(testCfg(), core.NewMemory(), time.Now()))
	if err := d.Run(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "another daemon") {
		t.Fatalf("%v", err)
	}
}

// --- pidfd watcher contract test (real child process) ---------------------------

func TestPidWatcherReportsExit(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("cannot start sleep:", err)
	}
	ch := make(chan int, 4)
	w, err := newPidWatcher(ch)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Watch(cmd.Process.Pid)
	w.Watch(cmd.Process.Pid) // idempotent
	select {
	case pid := <-ch:
		t.Fatalf("reported %d before it exited", pid)
	case <-time.After(50 * time.Millisecond):
	}
	cmd.Process.Kill()
	cmd.Wait()
	select {
	case pid := <-ch:
		if pid != cmd.Process.Pid {
			t.Fatalf("pid %d", pid)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("exit not reported")
	}
}

// --- real adapters refuse inside tests ------------------------------------------

func TestRealAdaptersRefuseInTests(t *testing.T) {
	if err := (ExecNotifier{}).Notify([]string{"notify-send", "x"}); err == nil {
		t.Fatal("ExecNotifier ran from a test")
	}
	if _, err := (ClaudeSummarizer{}).Summarize(context.Background(), "haiku", model.Profile{Dir: "/h/.claude", Default: true}, "p", "x"); err == nil {
		t.Fatal("ClaudeSummarizer ran from a test")
	}
	if err := (TmuxOptions{Client: tmux.New("")}).SetStatus("x"); err == nil {
		t.Fatal("TmuxOptions touched the default tmux server from a test")
	}
}

func TestWatchesEveryProfilesRegistryIncludingLaterOnes(t *testing.T) {
	h := start(t, testCfg())
	// A second profile appears later (discovered from a new process).
	work := model.Profile{Name: "work", Dir: filepath.Join(h.dir, "claude-work")}
	if err := os.MkdirAll(work.RegistryDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	h.f.set(func(f *fakes) { f.profiles = append(f.profiles, work) })
	h.client.Call(ipc.Request{Op: ipc.OpRefresh}, time.Second)
	// Its registry is now watched: a change there reaches the observer.
	p := filepath.Join(work.RegistryDir(), "4242.json")
	os.WriteFile(p, []byte("{}"), 0o600)
	eventually(t, "registry change in the new profile", func() bool {
		return h.f.get(func(f *fakes) bool {
			for _, x := range f.regFiles {
				if x == p {
					return true
				}
			}
			return false
		})
	})
	// Files outside any registry, or not named <pid>.json, are ignored.
	os.WriteFile(filepath.Join(work.RegistryDir(), "notes.txt"), nil, 0o600)
	time.Sleep(50 * time.Millisecond)
	if h.f.get(func(f *fakes) bool {
		for _, x := range f.regFiles {
			if strings.HasSuffix(x, "notes.txt") {
				return true
			}
		}
		return false
	}) {
		t.Fatal("a non-entry file reached the observer")
	}
}

func TestWithoutEnv(t *testing.T) {
	got := withoutEnv([]string{"A=1", "CLAUDE_CONFIG_DIR=/x", "CLAUDE_CONFIG_DIRX=2"}, "CLAUDE_CONFIG_DIR")
	if strings.Join(got, ",") != "A=1,CLAUDE_CONFIG_DIRX=2" {
		t.Fatal(got)
	}
}

func TestSummaryCommand(t *testing.T) {
	environ := []string{"HOME=/h", "CLAUDE_CONFIG_DIR=/h/.claude"}
	argv, env, err := summaryCommand("haiku", model.Profile{Dir: "/h/.claude-work"}, "the prompt", "the input", environ)
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "claude" || argv[1] != "-p" || argv[len(argv)-1] != "the input" {
		t.Fatalf("argv %q", argv)
	}
	for _, flag := range []string{"--no-session-persistence", "--strict-mcp-config", "--tools", "--setting-sources"} {
		found := false
		for _, a := range argv {
			found = found || a == flag
		}
		if !found {
			t.Errorf("missing %s", flag)
		}
	}
	want := []string{"HOME=/h", "CCCP_INTERNAL=1", "CLAUDE_CONFIG_DIR=/h/.claude-work"}
	if strings.Join(env, ",") != strings.Join(want, ",") {
		t.Fatalf("env %q: the session's profile must replace any inherited one", env)
	}
	// The default profile runs with the variable removed: set to ~/.claude,
	// Claude Code would use a blank ~/.claude/.claude.json.
	_, env, _ = summaryCommand("haiku", model.Profile{Dir: "/h/.claude", Default: true}, "p", "x",
		[]string{"HOME=/h", "CLAUDE_CONFIG_DIR=/h/.claude-work"})
	if strings.Join(env, ",") != "HOME=/h,CCCP_INTERNAL=1" {
		t.Fatalf("default profile env %q", env)
	}
	if environ[1] != "CLAUDE_CONFIG_DIR=/h/.claude" {
		t.Fatal("the daemon's environment was modified")
	}
	if _, _, err := summaryCommand("haiku", model.Profile{}, "p", "x", nil); err == nil {
		t.Fatal("a summary without a profile must be refused")
	}
}

// --- catalog indexing ---------------------------------------------------------------

func TestIndexRunsAtStartAndOnRefresh(t *testing.T) {
	h := start(t, testCfg())
	eventually(t, "index at start, then the catalog reread", func() bool {
		return h.f.get(func(f *fakes) bool { return f.indexRuns == 1 && f.catalogReads >= 1 })
	})
	h.client.Call(ipc.Request{Op: ipc.OpRefresh}, time.Second)
	eventually(t, "index on refresh", func() bool {
		return h.f.get(func(f *fakes) bool { return f.indexRuns == 2 })
	})
}

func TestIndexTriggersAreCoalesced(t *testing.T) {
	h := start(t, testCfg())
	eventually(t, "first index", func() bool { return h.f.get(func(f *fakes) bool { return f.indexRuns == 1 }) })
	hold := make(chan struct{})
	h.f.set(func(f *fakes) { f.indexHold = hold })
	// A slow run, and five more triggers while it runs.
	for i := 0; i < 6; i++ {
		h.client.Call(ipc.Request{Op: ipc.OpRefresh}, time.Second)
	}
	h.f.set(func(f *fakes) { f.indexHold = nil })
	close(hold)
	eventually(t, "one follow-up run", func() bool { return h.f.get(func(f *fakes) bool { return f.indexRuns == 3 }) })
	time.Sleep(100 * time.Millisecond)
	if n := h.f.get(func(f *fakes) bool { return f.indexRuns == 3 }); !n {
		t.Fatalf("triggers during a run must coalesce into one more run; runs=%d", h.f.indexRuns)
	}
}

func TestSessionEndTriggersIndex(t *testing.T) {
	h := start(t, testCfg())
	eventually(t, "first index", func() bool { return h.f.get(func(f *fakes) bool { return f.indexRuns == 1 }) })
	h.f.set(func(f *fakes) { f.ended = true })
	os.WriteFile(filepath.Join(h.dir, "sessions", "claude-s1.json"), []byte("{}"), 0o600)
	// The fake reports the end on every event of the write (create, write);
	// the gatherer reports it once. Either way: at least one more index.
	eventually(t, "index after SessionEnd", func() bool { return h.f.get(func(f *fakes) bool { return f.indexRuns >= 2 }) })
}

func TestSummarizeRequestForAClosedConversation(t *testing.T) {
	cfg := testCfg()
	cfg.Summaries = "always"
	h := start(t, cfg)
	h.f.set(func(f *fakes) { f.closedConv = true })
	h.client.Call(ipc.Request{Op: ipc.OpRefresh}, time.Second)
	if _, err := h.client.Call(ipc.Request{Op: ipc.OpSummarize, Key: "claude:old"}, time.Second); err != nil {
		t.Fatal(err)
	}
	eventually(t, "closed summary", func() bool {
		resp, err := h.client.Call(ipc.Request{Op: ipc.OpSnapshot}, time.Second)
		if err != nil {
			return false
		}
		for _, s := range resp.Snapshot.Sessions {
			if s.Key == "claude:old" && s.Summary != "" {
				return true
			}
		}
		return false
	})
}
