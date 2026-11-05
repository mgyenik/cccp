package gather

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cccp/internal/catalog"
	"cccp/internal/claudereg"
	"cccp/internal/core"
	"cccp/internal/model"
	"cccp/internal/procs"
	"cccp/internal/tmux"
	"cccp/internal/transcript"
)

// --- fakes -------------------------------------------------------------------

type fakeHooks struct {
	files map[string]model.HookState
	loads int
}

func (f *fakeHooks) LoadAll() (map[string]model.HookState, []error) {
	out := map[string]model.HookState{}
	for k, v := range f.files {
		out[k] = v
	}
	return out, []error{errors.New("bad.json: corrupt")}
}

func (f *fakeHooks) Load(p string) (model.HookState, error) {
	f.loads++
	s, ok := f.files[p]
	if !ok {
		return s, os.ErrNotExist
	}
	return s, nil
}

type fakeReg struct{ files map[string]claudereg.Entry }

func (f *fakeReg) ReadAll(dir string) []claudereg.Entry {
	var out []claudereg.Entry
	for p, e := range f.files {
		if filepath.Dir(p) == dir {
			e.Path = p
			out = append(out, e)
		}
	}
	return out
}

func (f *fakeReg) Read(p string) (claudereg.Entry, error) {
	e, ok := f.files[p]
	if !ok {
		return e, os.ErrNotExist
	}
	e.Path = p
	return e, nil
}

type fakeTmux struct {
	panes  []tmux.Pane
	err    error
	calls  int
	socket string
	// identity is the server's pid and start time, for a server with no panes.
	identPID   int
	identStart int64
	identCalls int
}

func (f *fakeTmux) ServerIdentity() (int, int64, error) {
	f.identCalls++
	return f.identPID, f.identStart, nil
}

func (f *fakeTmux) ListPanes() ([]tmux.Pane, error) { f.calls++; return f.panes, f.err }
func (f *fakeTmux) SocketPath() (string, error)     { return f.socket, nil }
func (f *fakeTmux) ListClients() ([]tmux.ClientInfo, error) {
	return []tmux.ClientInfo{{Name: "c", Pane: "%1", Focused: true}}, nil
}

type fakeProcs struct {
	table    procs.Table
	alive    map[int]bool
	cmdline  map[int][]string
	cwd      map[int]string
	env      map[int]map[string]string
	envReads int
}

func (f *fakeProcs) Snapshot() procs.Table              { return f.table }
func (f *fakeProcs) Alive(pid int, start uint64) bool   { return f.alive[pid] }
func (f *fakeProcs) Cmdline(pid int) []string           { return f.cmdline[pid] }
func (f *fakeProcs) Cwd(pid int) string                 { return f.cwd[pid] }
func (f *fakeProcs) Environ(pid int, key string) string { f.envReads++; return f.env[pid][key] }
func (f *fakeProcs) StartWall(ticks uint64) time.Time   { return time.Unix(int64(ticks), 0) }

type fakeTranscripts struct {
	claude map[string]transcript.ClaudeMeta
	agy    map[string]transcript.AgyMeta
	read   []string
}

func (f *fakeTranscripts) Claude(p string) (transcript.ClaudeMeta, error) {
	f.read = append(f.read, p)
	m, ok := f.claude[p]
	if !ok {
		return m, os.ErrNotExist
	}
	return m, nil
}

func (f *fakeTranscripts) Agy(p string) (transcript.AgyMeta, error) {
	f.read = append(f.read, p)
	m, ok := f.agy[p]
	if !ok {
		return m, os.ErrNotExist
	}
	return m, nil
}

type fakeFiles struct {
	claude      map[string]string           // sid → path
	projects    map[string][]TranscriptFile // configDir+"|"+cwd → files
	profileDirs []string
	finds       []string // "sid@configDir"
}

func (f *fakeFiles) FindClaude(sid, configDir, cwd string) string {
	f.finds = append(f.finds, sid+"@"+configDir)
	return f.claude[sid]
}
func (f *fakeFiles) ListProject(configDir, cwd string) ([]TranscriptFile, error) {
	l, ok := f.projects[configDir+"|"+cwd]
	if !ok {
		return nil, os.ErrNotExist
	}
	return l, nil
}
func (f *fakeFiles) AgyTranscript(id string) string { return "/agy/" + id }
func (f *fakeFiles) ProfileDirs() []string          { return f.profileDirs }

type fakeAgy struct{}

func (fakeAgy) All() map[string]model.AgyConversation { return nil }
func (fakeAgy) LastExecution(id string) model.AgyExecution {
	if id == "g1" {
		return model.AgyExecution{Known: true, Reason: model.AgyUserCanceled, LastStep: 7}
	}
	return model.AgyExecution{}
}

type fakeCatalog struct {
	docs []catalog.Doc
	err  error
}

func (f *fakeCatalog) Docs() ([]catalog.Doc, error) { return f.docs, f.err }

type fakePower bool

func (p fakePower) OnBattery() bool { return bool(p) }

type world struct {
	hooks *fakeHooks
	reg   *fakeReg
	tmux  *fakeTmux
	procs *fakeProcs
	tr    *fakeTranscripts
	files *fakeFiles
	cat   *fakeCatalog
	g     *Gatherer
}

func newWorld() *world {
	w := &world{
		hooks: &fakeHooks{files: map[string]model.HookState{}},
		reg:   &fakeReg{files: map[string]claudereg.Entry{}},
		tmux:  &fakeTmux{},
		procs: &fakeProcs{alive: map[int]bool{}, cmdline: map[int][]string{}, cwd: map[int]string{}, env: map[int]map[string]string{}},
		tr:    &fakeTranscripts{claude: map[string]transcript.ClaudeMeta{}, agy: map[string]transcript.AgyMeta{}},
		files: &fakeFiles{claude: map[string]string{}, projects: map[string][]TranscriptFile{}},
		cat:   &fakeCatalog{},
	}
	w.g = New(Deps{Hooks: w.hooks, Registry: w.reg, Tmux: w.tmux, Procs: w.procs, Transcripts: w.tr,
		Files: w.files, AgyDB: fakeAgy{}, Power: fakePower(true), Catalog: w.cat, Hostname: "host",
		Home: "/h", DefaultClaudeDir: "/h/.claude"})
	return w
}

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// --- tests -------------------------------------------------------------------

func TestReloadAndLiveness(t *testing.T) {
	w := newWorld()
	w.hooks.files["/s/a.json"] = model.HookState{Agent: model.Claude, SessionID: "a", PID: 10, PIDStart: 1}
	w.hooks.files["/s/b.json"] = model.HookState{Agent: model.Claude, SessionID: "b", PID: 11, PIDStart: 1}
	w.hooks.files["/s/c.json"] = model.HookState{Agent: model.Claude, SessionID: "c"} // no pid
	w.reg.files["/h/.claude/sessions/12.json"] = claudereg.Entry{PID: 12, SessionID: "r", ProcStart: "5", Kind: "interactive"}
	w.procs.alive[10], w.procs.alive[12] = true, true
	w.g.Reload()
	obs := w.g.Observe(now)
	alive := map[string]bool{}
	for _, h := range obs.Hooks {
		alive[h.State.SessionID] = h.ProcAlive
	}
	if !reflect.DeepEqual(alive, map[string]bool{"a": true, "b": false, "c": false}) {
		t.Fatalf("hook liveness %v", alive)
	}
	if len(obs.Registry) != 1 || !obs.Registry[0].Alive || obs.Registry[0].Path != "/h/.claude/sessions/12.json" || obs.Registry[0].ConfigDir != "/h/.claude" {
		t.Fatalf("registry %+v", obs.Registry)
	}
	if len(obs.Problems) != 1 || !obs.OnBattery || obs.Hostname != "host" {
		t.Fatalf("problems/power/host: %+v", obs)
	}
}

func TestHookFileChangedRediscoverRule(t *testing.T) {
	w := newWorld()
	p := "/s/a.json"
	w.hooks.files[p] = model.HookState{SessionID: "a", PID: 10, Pane: "%1"}
	if !w.g.HookFileChanged(p).Rediscover {
		t.Fatal("a new session must trigger rediscovery")
	}
	w.hooks.files[p] = model.HookState{SessionID: "a", PID: 10, Pane: "%1", Activity: model.Working}
	if w.g.HookFileChanged(p).Rediscover {
		t.Fatal("an activity change alone must not trigger rediscovery")
	}
	w.hooks.files[p] = model.HookState{SessionID: "a", PID: 20, Pane: "%1"}
	if !w.g.HookFileChanged(p).Rediscover {
		t.Fatal("a new process must trigger rediscovery")
	}
	w.hooks.files[p] = model.HookState{SessionID: "a", PID: 20, Pane: "%2"}
	if !w.g.HookFileChanged(p).Rediscover {
		t.Fatal("a new pane must trigger rediscovery")
	}
	delete(w.hooks.files, p)
	if !w.g.HookFileChanged(p).Rediscover {
		t.Fatal("a removed session must trigger rediscovery")
	}
	if len(w.g.Observe(now).Hooks) != 0 {
		t.Fatal("removed file still observed")
	}
}

func TestRegistryFileChangedRediscoverRule(t *testing.T) {
	w := newWorld()
	p := "/r/12.json"
	w.reg.files[p] = claudereg.Entry{PID: 12, SessionID: "a", Status: "idle"}
	if !w.g.RegistryFileChanged(p) {
		t.Fatal("new entry")
	}
	w.reg.files[p] = claudereg.Entry{PID: 12, SessionID: "a", Status: "busy"}
	if w.g.RegistryFileChanged(p) {
		t.Fatal("status change alone")
	}
	w.reg.files[p] = claudereg.Entry{PID: 12, SessionID: "b", Status: "busy"}
	if !w.g.RegistryFileChanged(p) {
		t.Fatal("new session on the process (/clear)")
	}
}

func TestTmuxFailureIsReported(t *testing.T) {
	w := newWorld()
	w.tmux.err = errors.New("no server")
	w.g.RefreshTmux()
	obs := w.g.Observe(now)
	if obs.Tmux.OK || obs.Tmux.Err != "no server" {
		t.Fatalf("%+v", obs.Tmux)
	}
}

func TestDiscoverBuildsAgentProcesses(t *testing.T) {
	w := newWorld()
	w.tmux.panes = []tmux.Pane{{ID: "%1", PID: 100}, {ID: "%2", PID: 200}}
	w.procs.table = procs.Table{
		ByPID: map[int]procs.Stat{
			100: {PID: 100, Comm: "bash"}, 101: {PID: 101, Comm: "claude", StartTime: 7},
			102: {PID: 102, Comm: "claude", StartTime: 8}, // claude -p run by the session
			200: {PID: 200, Comm: "agy", StartTime: 9},
		},
		Children: map[int][]int{100: {101}, 101: {102}},
	}
	w.procs.cmdline[101] = []string{"claude"}
	w.procs.cmdline[102] = []string{"claude", "-p", "x"}
	w.procs.cwd[101] = "/w"
	w.g.RefreshTmux()
	w.g.Discover()
	obs := w.g.Observe(now)
	if len(obs.Agents) != 3 {
		t.Fatalf("agents %+v", obs.Agents)
	}
	a := obs.Agents[0]
	if a.PID != 101 || a.Pane != "%1" || a.Depth != 1 || a.Cwd != "/w" || a.Agent != model.Claude || !a.StartedAt.Equal(time.Unix(7, 0)) {
		t.Fatalf("%+v", a)
	}
	if obs.Agents[1].PID != 102 || obs.Agents[1].Depth != 2 || obs.Agents[2].Agent != model.Agy || obs.Agents[2].Depth != 0 {
		t.Fatalf("%+v", obs.Agents)
	}
}

func TestObserveReadsOnlyNeededTranscripts(t *testing.T) {
	w := newWorld()
	// A hook session that reports its transcript path, and one that doesn't.
	w.hooks.files["/s/a.json"] = model.HookState{Agent: model.Claude, SessionID: "a", PID: 10, TranscriptPath: "/t/a.jsonl"}
	w.hooks.files["/s/b.json"] = model.HookState{Agent: model.Claude, SessionID: "b", PID: 11, Cwd: "/w"}
	w.procs.alive[10], w.procs.alive[11] = true, true
	w.tr.claude["/t/a.jsonl"] = transcript.ClaudeMeta{AITitle: "A"}
	w.tr.claude["/t/b.jsonl"] = transcript.ClaudeMeta{AITitle: "B"}
	w.tr.claude["/t/unrelated.jsonl"] = transcript.ClaudeMeta{AITitle: "X"}
	w.files.claude["b"] = "/t/b.jsonl"
	w.g.Reload()
	obs := w.g.Observe(now)
	if obs.ClaudeTranscripts["a"].Meta.AITitle != "A" || obs.ClaudeTranscripts["b"].Meta.AITitle != "B" {
		t.Fatalf("%+v", obs.ClaudeTranscripts)
	}
	if !reflect.DeepEqual(w.tr.read, []string{"/t/a.jsonl", "/t/b.jsonl"}) {
		t.Fatalf("read %v", w.tr.read)
	}
	if len(w.files.finds) != 1 || w.files.finds[0] != "b@/h/.claude" {
		t.Fatalf("the hinted path must not be searched for: %v", w.files.finds)
	}
	// A found path is cached.
	w.g.Observe(now.Add(time.Hour))
	if len(w.files.finds) != 1 {
		t.Fatalf("found path not cached: %v", w.files.finds)
	}
}

func TestMissingTranscriptRetriedAfterAMinute(t *testing.T) {
	w := newWorld()
	w.hooks.files["/s/b.json"] = model.HookState{Agent: model.Claude, SessionID: "b", PID: 11}
	w.procs.alive[11] = true
	w.g.Reload()
	w.g.Observe(now)
	w.g.Observe(now.Add(30 * time.Second))
	if len(w.files.finds) != 1 {
		t.Fatalf("a miss should be cached for a minute: %v", w.files.finds)
	}
	w.files.claude["b"] = "/t/b.jsonl"
	w.tr.claude["/t/b.jsonl"] = transcript.ClaudeMeta{AITitle: "B"}
	obs := w.g.Observe(now.Add(2 * time.Minute))
	if len(w.files.finds) != 2 || obs.ClaudeTranscripts["b"].Meta.AITitle != "B" {
		t.Fatalf("finds=%v %+v", w.files.finds, obs.ClaudeTranscripts)
	}
}

func TestProjectsListedForUnmanagedTitleMatch(t *testing.T) {
	w := newWorld()
	w.tmux.panes = []tmux.Pane{{ID: "%1", PID: 100, Title: "✳ Lost session"}}
	w.procs.table = procs.Table{ByPID: map[int]procs.Stat{101: {PID: 101, Comm: "claude"}}, Children: map[int][]int{100: {101}}}
	w.procs.cmdline[101] = []string{"claude"}
	w.procs.cwd[101] = "/w"
	w.files.projects["/h/.claude|/w"] = []TranscriptFile{{SessionID: "x", Path: "/p/x.jsonl"}, {SessionID: "y", Path: "/p/y.jsonl"}}
	w.tr.claude["/p/x.jsonl"] = transcript.ClaudeMeta{AITitle: "Lost session"}
	w.tr.claude["/p/y.jsonl"] = transcript.ClaudeMeta{AITitle: "Other"}
	w.g.RefreshTmux()
	w.g.Discover()
	obs := w.g.Observe(now)
	if l := obs.Projects[core.ProjectKey{ConfigDir: "/h/.claude", Cwd: "/w"}]; len(l) != 2 || l[0].Meta.AITitle != "Lost session" {
		t.Fatalf("%+v", obs.Projects)
	}
}

// --- profiles ----------------------------------------------------------------------

func names(ps []model.Profile) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name+"="+p.Dir)
	}
	return out
}

func TestProfilesDiscoveredFromDirectoriesAndReadAtOnce(t *testing.T) {
	w := newWorld()
	w.files.profileDirs = []string{"/h/.claude-work"}
	w.reg.files["/h/.claude-work/sessions/7.json"] = claudereg.Entry{PID: 7, SessionID: "work1", ProcStart: "1", Kind: "interactive"}
	w.procs.alive[7] = true
	w.g.Reload()
	obs := w.g.Observe(now)
	if got := names(obs.Profiles); !reflect.DeepEqual(got, []string{"default=/h/.claude", "work=/h/.claude-work"}) {
		t.Fatalf("profiles %v", got)
	}
	if len(obs.Registry) != 1 || obs.Registry[0].ConfigDir != "/h/.claude-work" {
		t.Fatalf("registry %+v", obs.Registry)
	}
}

func TestProfileDiscoveredFromAProcessEnvironment(t *testing.T) {
	w := newWorld()
	w.reg.files["/h/.claude-x/sessions/101.json"] = claudereg.Entry{PID: 101, SessionID: "x1", ProcStart: "7", Kind: "interactive"}
	w.procs.alive[101] = true
	w.tmux.panes = []tmux.Pane{{ID: "%1", PID: 100}}
	w.procs.table = procs.Table{ByPID: map[int]procs.Stat{101: {PID: 101, Comm: "claude", StartTime: 7}}, Children: map[int][]int{100: {101}}}
	w.procs.env[101] = map[string]string{"CLAUDE_CONFIG_DIR": "~/.claude-x/"}
	w.g.Reload()
	w.g.RefreshTmux()
	w.g.Discover()
	obs := w.g.Observe(now)
	if obs.Agents[0].ConfigDir != "/h/.claude-x" {
		t.Fatalf("agent config dir %q (want normalised)", obs.Agents[0].ConfigDir)
	}
	if len(obs.Registry) != 1 || obs.Registry[0].ConfigDir != "/h/.claude-x" {
		t.Fatalf("the new profile's registry was not read: %+v", obs.Registry)
	}
}

func TestProfileDiscoveredFromHookStateAndTranscriptLookedUpThere(t *testing.T) {
	w := newWorld()
	p := "/s/w.json"
	w.hooks.files[p] = model.HookState{Agent: model.Claude, SessionID: "w", PID: 11, ConfigDir: "/h/.claude-work"}
	w.procs.alive[11] = true
	w.reg.files["/h/.claude-work/sessions/11.json"] = claudereg.Entry{PID: 11, SessionID: "w", ProcStart: "1", Kind: "interactive"}
	w.g.HookFileChanged(p)
	obs := w.g.Observe(now)
	if len(obs.Profiles) != 2 || len(obs.Registry) != 1 {
		t.Fatalf("profiles %v registry %+v", names(obs.Profiles), obs.Registry)
	}
	if len(w.files.finds) != 1 || w.files.finds[0] != "w@/h/.claude-work" {
		t.Fatalf("transcript looked up in the wrong profile: %v", w.files.finds)
	}
}

func TestHookChangeReportsTheSessionEnding(t *testing.T) {
	w := newWorld()
	p := "/s/a.json"
	w.hooks.files[p] = model.HookState{SessionID: "a", PID: 10, Activity: model.Working}
	if c := w.g.HookFileChanged(p); c.Ended {
		t.Fatal("not ended yet")
	}
	w.hooks.files[p] = model.HookState{SessionID: "a", PID: 10, Activity: model.Ended}
	if c := w.g.HookFileChanged(p); !c.Ended {
		t.Fatal("the end must be reported")
	}
	if c := w.g.HookFileChanged(p); c.Ended {
		t.Fatal("reported twice")
	}
}

func TestCatalogIsReadOnlyOnRefresh(t *testing.T) {
	w := newWorld()
	w.cat.docs = []catalog.Doc{{Agent: model.Claude, SessionID: "c1", ConfigDir: "~/.claude-work", Title: "Old work", Turns: 3, Source: "/t/c1.jsonl"}}
	if obs := w.g.Observe(now); len(obs.Conversations) != 0 {
		t.Fatal("conversations before any refresh")
	}
	w.g.RefreshCatalog()
	obs := w.g.Observe(now)
	if len(obs.Conversations) != 1 || obs.Conversations[0].ConfigDir != "/h/.claude-work" || obs.Conversations[0].Turns != 3 {
		t.Fatalf("%+v", obs.Conversations)
	}
	w.cat.err = errors.New("disk on fire")
	w.g.RefreshCatalog()
	obs = w.g.Observe(now)
	if len(obs.Conversations) != 1 || len(obs.Problems) == 0 {
		t.Fatalf("a failed refresh keeps the last catalog and reports: %+v", obs.Problems)
	}
}

func TestTheTmuxServersAreRecorded(t *testing.T) {
	w := newWorld()
	w.tmux.socket = "/tmp/tmux-1000/default"
	w.reg.files["/h/.claude/sessions/12.json"] = claudereg.Entry{PID: 12, SessionID: "r", ProcStart: "5", Tmux: "work:@2.%2"}
	w.reg.files["/h/.claude/sessions/13.json"] = claudereg.Entry{PID: 13, SessionID: "q", ProcStart: "5"} // not in tmux
	w.procs.alive[12], w.procs.alive[13] = true, true
	w.procs.env[12] = map[string]string{"TMUX": "/tmp/tmux-1000/work,999,0"}
	w.g.Reload()
	w.g.RefreshTmux()
	obs := w.g.Observe(now)
	if w.procs.envReads != 1 {
		t.Fatalf("%d environment reads: only a process whose entry names a pane needs one", w.procs.envReads)
	}
	if obs.Tmux.Socket != "/tmp/tmux-1000/default" {
		t.Fatalf("our server %q", obs.Tmux.Socket)
	}
	socks := map[string]string{}
	for _, r := range obs.Registry {
		socks[r.Entry.SessionID] = r.TmuxSocket
	}
	if socks["r"] != "/tmp/tmux-1000/work" || socks["q"] != "" {
		t.Fatalf("registry servers %v", socks)
	}
	// A process's environment never changes: it is read once, not per
	// observation (which happens on every event).
	reads := w.procs.envReads
	w.g.Observe(now)
	w.g.Observe(now)
	if w.procs.envReads != reads {
		t.Fatalf("environment reread: %d → %d", reads, w.procs.envReads)
	}
	// A registered process that has exited: its pid may now be another
	// process's, whose environment says nothing about this session.
	w.reg.files["/h/.claude/sessions/14.json"] = claudereg.Entry{PID: 14, SessionID: "gone", ProcStart: "5", Tmux: "x:@1.%1"}
	w.procs.env[14] = map[string]string{"TMUX": "/tmp/tmux-1000/someone-else,1,0"}
	w.g.RegistryFileChanged("/h/.claude/sessions/14.json")
	for _, r := range w.g.Observe(now).Registry {
		if r.Entry.SessionID == "gone" && r.TmuxSocket != "" {
			t.Fatalf("server read from a reused pid: %q", r.TmuxSocket)
		}
	}
	// A new process under the same pid is a different process.
	w.reg.files["/h/.claude/sessions/12.json"] = claudereg.Entry{PID: 12, SessionID: "r2", ProcStart: "6", Tmux: "main:@0.%0"}
	w.procs.env[12] = map[string]string{"TMUX": "/tmp/tmux-1000/default,1,0"}
	w.g.RegistryFileChanged("/h/.claude/sessions/12.json")
	for _, r := range w.g.Observe(now).Registry {
		if r.Entry.SessionID == "r2" && r.TmuxSocket != "/tmp/tmux-1000/default" {
			t.Fatalf("stale server for a new process: %q", r.TmuxSocket)
		}
	}
}

func TestServerIdentity(t *testing.T) {
	w := newWorld()
	w.tmux.panes = []tmux.Pane{{ID: "%1", Session: "s", ServerPID: 7, ServerStart: 70}}
	w.tmux.identPID, w.tmux.identStart = 9, 90
	w.g.RefreshTmux()
	obs := w.g.Observe(now)
	if obs.Tmux.ServerPID != 7 || obs.Tmux.ServerStart != 70 || w.tmux.identCalls != 0 {
		t.Fatalf("from the panes, with no extra command: %+v, %d calls", obs.Tmux, w.tmux.identCalls)
	}
	w.tmux.panes = nil // a server with no session (exit-empty off)
	w.g.RefreshTmux()
	obs = w.g.Observe(now)
	if !obs.Tmux.OK || obs.Tmux.ServerPID != 9 || obs.Tmux.ServerStart != 90 {
		t.Fatalf("empty server: %+v", obs.Tmux)
	}
	w.tmux.err = tmux.ErrNoServer
	w.g.RefreshTmux()
	if obs := w.g.Observe(now); obs.Tmux.OK || !obs.Tmux.NoServer {
		t.Fatalf("no server: %+v", obs.Tmux)
	}
}

// An agy session's transcript comes with how its latest execution ended.
func TestObserveReadsAgyExecutions(t *testing.T) {
	w := newWorld()
	w.hooks.files["/s/g1.json"] = model.HookState{Agent: model.Agy, SessionID: "g1", PID: 10, TranscriptPath: "/t/g1.jsonl"}
	w.procs.alive[10] = true
	w.tr.agy["/t/g1.jsonl"] = transcript.AgyMeta{LastStepIndex: 7}
	w.g.Reload()
	obs := w.g.Observe(now)
	if got := obs.AgyTranscripts["g1"]; got.Meta.LastStepIndex != 7 || got.Exec != (model.AgyExecution{Known: true, Reason: model.AgyUserCanceled, LastStep: 7}) {
		t.Fatalf("%+v", got)
	}
}
