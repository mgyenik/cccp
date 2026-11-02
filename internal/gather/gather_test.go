package gather

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"cccp/internal/agydb"
	"cccp/internal/claudereg"
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

func (f *fakeReg) ReadAll() []claudereg.Entry {
	var out []claudereg.Entry
	for p, e := range f.files {
		e.Path = p
		out = append(out, e)
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
	panes []tmux.Pane
	err   error
	calls int
}

func (f *fakeTmux) ListPanes() ([]tmux.Pane, error) { f.calls++; return f.panes, f.err }
func (f *fakeTmux) ListClients() ([]tmux.ClientInfo, error) {
	return []tmux.ClientInfo{{Name: "c", Pane: "%1", Focused: true}}, nil
}

type fakeProcs struct {
	table   procs.Table
	alive   map[int]bool
	cmdline map[int][]string
	cwd     map[int]string
}

func (f *fakeProcs) Snapshot() procs.Table            { return f.table }
func (f *fakeProcs) Alive(pid int, start uint64) bool { return f.alive[pid] }
func (f *fakeProcs) Cmdline(pid int) []string         { return f.cmdline[pid] }
func (f *fakeProcs) Cwd(pid int) string               { return f.cwd[pid] }
func (f *fakeProcs) StartWall(ticks uint64) time.Time { return time.Unix(int64(ticks), 0) }

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
	claude   map[string]string // sid → path
	projects map[string][]TranscriptFile
	finds    int
}

func (f *fakeFiles) FindClaude(sid, cwd string) string { f.finds++; return f.claude[sid] }
func (f *fakeFiles) ListProject(cwd string) ([]TranscriptFile, error) {
	l, ok := f.projects[cwd]
	if !ok {
		return nil, os.ErrNotExist
	}
	return l, nil
}
func (f *fakeFiles) AgyTranscript(id string) string { return "/agy/" + id }

type fakeAgy struct{}

func (fakeAgy) All() map[string]agydb.Conversation { return nil }

type fakePower bool

func (p fakePower) OnBattery() bool { return bool(p) }

type world struct {
	hooks *fakeHooks
	reg   *fakeReg
	tmux  *fakeTmux
	procs *fakeProcs
	tr    *fakeTranscripts
	files *fakeFiles
	g     *Gatherer
}

func newWorld() *world {
	w := &world{
		hooks: &fakeHooks{files: map[string]model.HookState{}},
		reg:   &fakeReg{files: map[string]claudereg.Entry{}},
		tmux:  &fakeTmux{},
		procs: &fakeProcs{alive: map[int]bool{}, cmdline: map[int][]string{}, cwd: map[int]string{}},
		tr:    &fakeTranscripts{claude: map[string]transcript.ClaudeMeta{}, agy: map[string]transcript.AgyMeta{}},
		files: &fakeFiles{claude: map[string]string{}, projects: map[string][]TranscriptFile{}},
	}
	w.g = New(Deps{Hooks: w.hooks, Registry: w.reg, Tmux: w.tmux, Procs: w.procs, Transcripts: w.tr,
		Files: w.files, AgyDB: fakeAgy{}, Power: fakePower(true), Hostname: "host"})
	return w
}

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// --- tests -------------------------------------------------------------------

func TestReloadAndLiveness(t *testing.T) {
	w := newWorld()
	w.hooks.files["/s/a.json"] = model.HookState{Agent: model.Claude, SessionID: "a", PID: 10, PIDStart: 1}
	w.hooks.files["/s/b.json"] = model.HookState{Agent: model.Claude, SessionID: "b", PID: 11, PIDStart: 1}
	w.hooks.files["/s/c.json"] = model.HookState{Agent: model.Claude, SessionID: "c"} // no pid
	w.reg.files["/r/12.json"] = claudereg.Entry{PID: 12, SessionID: "r", ProcStart: "5", Kind: "interactive"}
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
	if len(obs.Registry) != 1 || !obs.Registry[0].Alive || obs.Registry[0].Path != "/r/12.json" {
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
	if !w.g.HookFileChanged(p) {
		t.Fatal("a new session must trigger rediscovery")
	}
	w.hooks.files[p] = model.HookState{SessionID: "a", PID: 10, Pane: "%1", Activity: model.Working}
	if w.g.HookFileChanged(p) {
		t.Fatal("an activity change alone must not trigger rediscovery")
	}
	w.hooks.files[p] = model.HookState{SessionID: "a", PID: 20, Pane: "%1"}
	if !w.g.HookFileChanged(p) {
		t.Fatal("a new process must trigger rediscovery")
	}
	w.hooks.files[p] = model.HookState{SessionID: "a", PID: 20, Pane: "%2"}
	if !w.g.HookFileChanged(p) {
		t.Fatal("a new pane must trigger rediscovery")
	}
	delete(w.hooks.files, p)
	if !w.g.HookFileChanged(p) {
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
	if w.files.finds != 1 {
		t.Fatalf("the hinted path must not be searched for; finds=%d", w.files.finds)
	}
	// A found path is cached.
	w.g.Observe(now.Add(time.Hour))
	if w.files.finds != 1 {
		t.Fatalf("found path not cached: finds=%d", w.files.finds)
	}
}

func TestMissingTranscriptRetriedAfterAMinute(t *testing.T) {
	w := newWorld()
	w.hooks.files["/s/b.json"] = model.HookState{Agent: model.Claude, SessionID: "b", PID: 11}
	w.procs.alive[11] = true
	w.g.Reload()
	w.g.Observe(now)
	w.g.Observe(now.Add(30 * time.Second))
	if w.files.finds != 1 {
		t.Fatalf("a miss should be cached for a minute; finds=%d", w.files.finds)
	}
	w.files.claude["b"] = "/t/b.jsonl"
	w.tr.claude["/t/b.jsonl"] = transcript.ClaudeMeta{AITitle: "B"}
	obs := w.g.Observe(now.Add(2 * time.Minute))
	if w.files.finds != 2 || obs.ClaudeTranscripts["b"].Meta.AITitle != "B" {
		t.Fatalf("finds=%d %+v", w.files.finds, obs.ClaudeTranscripts)
	}
}

func TestProjectsListedForUnmanagedTitleMatch(t *testing.T) {
	w := newWorld()
	w.tmux.panes = []tmux.Pane{{ID: "%1", PID: 100, Title: "✳ Lost session"}}
	w.procs.table = procs.Table{ByPID: map[int]procs.Stat{101: {PID: 101, Comm: "claude"}}, Children: map[int][]int{100: {101}}}
	w.procs.cmdline[101] = []string{"claude"}
	w.procs.cwd[101] = "/w"
	w.files.projects["/w"] = []TranscriptFile{{SessionID: "x", Path: "/p/x.jsonl"}, {SessionID: "y", Path: "/p/y.jsonl"}}
	w.tr.claude["/p/x.jsonl"] = transcript.ClaudeMeta{AITitle: "Lost session"}
	w.tr.claude["/p/y.jsonl"] = transcript.ClaudeMeta{AITitle: "Other"}
	w.g.RefreshTmux()
	w.g.Discover()
	obs := w.g.Observe(now)
	if l := obs.Projects["/w"]; len(l) != 2 || l[0].Meta.AITitle != "Lost session" {
		t.Fatalf("%+v", obs.Projects)
	}
}
