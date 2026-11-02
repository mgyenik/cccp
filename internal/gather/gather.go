// Package gather is the read side of the imperative shell: it turns the
// world (hook state files, Claude's registry, tmux, /proc, transcripts,
// agy's database) into a core.Observation.
//
// A Gatherer keeps the last view of each source and refreshes only what an
// event touched. Every source is an interface, so the bookkeeping here is
// tested with fakes; the real adapters are tested against sandboxed real
// resources in their own packages.
package gather

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cccp/internal/agydb"
	"cccp/internal/claudereg"
	"cccp/internal/core"
	"cccp/internal/model"
	"cccp/internal/procs"
	"cccp/internal/tmux"
	"cccp/internal/transcript"
)

// HookStore reads hook state files.
type HookStore interface {
	LoadAll() (map[string]model.HookState, []error)
	Load(path string) (model.HookState, error)
}

// Registry reads Claude Code's session registry.
type Registry interface {
	ReadAll() []claudereg.Entry
	Read(path string) (claudereg.Entry, error)
}

// TmuxReader lists panes and clients.
type TmuxReader interface {
	ListPanes() ([]tmux.Pane, error)
	ListClients() ([]tmux.ClientInfo, error)
}

// Procs reads process information.
type Procs interface {
	Snapshot() procs.Table
	Alive(pid int, start uint64) bool
	Cmdline(pid int) []string
	Cwd(pid int) string
	StartWall(ticks uint64) time.Time
}

// Transcripts parses transcripts (incrementally, by path).
type Transcripts interface {
	Claude(path string) (transcript.ClaudeMeta, error)
	Agy(path string) (transcript.AgyMeta, error)
}

// TranscriptFile is a transcript file found on disk.
type TranscriptFile struct {
	SessionID string
	Path      string
	ModTime   time.Time
}

// Files locates transcripts.
type Files interface {
	// FindClaude returns the transcript of a Claude session, looking in
	// cwd's project directory first; "" when there is none.
	FindClaude(sessionID, cwd string) string
	// ListProject lists the transcripts in cwd's project directory.
	ListProject(cwd string) ([]TranscriptFile, error)
	// AgyTranscript is the transcript path of an agy conversation.
	AgyTranscript(conversationID string) string
}

// AgyDB reads agy's conversation summaries.
type AgyDB interface {
	All() map[string]agydb.Conversation
}

// Power reports the power source.
type Power interface {
	OnBattery() bool
}

// Deps are a Gatherer's sources.
type Deps struct {
	Hooks       HookStore
	Registry    Registry
	Tmux        TmuxReader
	Procs       Procs
	Transcripts Transcripts
	Files       Files
	AgyDB       AgyDB
	Power       Power
	Hostname    string
}

// Gatherer holds the last view of every source.
type Gatherer struct {
	d Deps

	hooks    map[string]model.HookState // by path
	hookErrs []string
	reg      map[string]claudereg.Entry // by path
	tmux     core.TmuxView
	agents   []core.AgentProc

	// claudePaths caches transcript locations; a miss is retried after
	// missRetry so a transcript created later is found.
	claudePaths map[string]pathLookup
}

type pathLookup struct {
	path    string
	checked time.Time
}

// missRetry bounds how long a "no transcript yet" lookup is cached.
const missRetry = time.Minute

// New creates a Gatherer with nothing loaded; call Reload, RefreshTmux and
// Discover before the first Observe.
func New(d Deps) *Gatherer {
	return &Gatherer{d: d, hooks: map[string]model.HookState{}, reg: map[string]claudereg.Entry{}, claudePaths: map[string]pathLookup{}}
}

// Reload rereads every hook state file and registry entry.
func (g *Gatherer) Reload() {
	all, errs := g.d.Hooks.LoadAll()
	g.hooks = all
	g.hookErrs = g.hookErrs[:0]
	for _, err := range errs {
		g.hookErrs = append(g.hookErrs, err.Error())
	}
	g.reg = map[string]claudereg.Entry{}
	for _, e := range g.d.Registry.ReadAll() {
		g.reg[e.Path] = e
	}
}

// HookFileChanged rereads one hook state file. It reports whether processes
// should be rediscovered: a session appeared or disappeared, or moved to
// another process or pane.
func (g *Gatherer) HookFileChanged(path string) bool {
	before, had := g.hooks[path]
	after, err := g.d.Hooks.Load(path)
	has := err == nil
	if has {
		g.hooks[path] = after
	} else {
		delete(g.hooks, path)
	}
	return had != has || before.PID != after.PID || before.Pane != after.Pane
}

// RegistryFileChanged rereads one registry file; the result is as for
// HookFileChanged.
func (g *Gatherer) RegistryFileChanged(path string) bool {
	before, had := g.reg[path]
	after, err := g.d.Registry.Read(path)
	has := err == nil
	if has {
		g.reg[path] = after
	} else {
		delete(g.reg, path)
	}
	return had != has || before.SessionID != after.SessionID || before.PID != after.PID || before.Tmux != after.Tmux
}

// RefreshTmux rereads panes and clients.
func (g *Gatherer) RefreshTmux() {
	panes, err := g.d.Tmux.ListPanes()
	if err != nil {
		g.tmux = core.TmuxView{Err: err.Error()}
		return
	}
	clients, err := g.d.Tmux.ListClients()
	if err != nil {
		clients = nil
	}
	g.tmux = core.TmuxView{OK: true, Panes: panes, Clients: clients}
}

// Discover scans /proc for agent processes under the known panes.
func (g *Gatherer) Discover() {
	g.agents = nil
	if len(g.tmux.Panes) == 0 {
		return
	}
	t := g.d.Procs.Snapshot()
	for _, p := range g.tmux.Panes {
		for _, f := range t.AgentsBelow(p.PID) {
			g.agents = append(g.agents, core.AgentProc{
				PID:       f.Stat.PID,
				Start:     f.Stat.StartTime,
				StartedAt: g.d.Procs.StartWall(f.Stat.StartTime),
				Agent:     model.Agent(f.Agent),
				Pane:      p.ID,
				Depth:     f.Depth,
				Cmdline:   g.d.Procs.Cmdline(f.Stat.PID),
				Cwd:       g.d.Procs.Cwd(f.Stat.PID),
			})
		}
	}
}

// Observe assembles an observation from the current view, reading the
// transcripts core.Plan asks for.
func (g *Gatherer) Observe(now time.Time) core.Observation {
	obs := core.Observation{
		Tmux:             g.tmux,
		Agents:           g.agents,
		OnBattery:        g.d.Power.OnBattery(),
		Hostname:         g.d.Hostname,
		AgyConversations: g.d.AgyDB.All(),
		Problems:         append([]string(nil), g.hookErrs...),
	}
	for _, p := range sortedKeys(g.hooks) {
		st := g.hooks[p]
		obs.Hooks = append(obs.Hooks, core.HookRecord{Path: p, State: st, ProcAlive: st.PID != 0 && g.d.Procs.Alive(st.PID, st.PIDStart)})
	}
	for _, p := range sortedKeys(g.reg) {
		e := g.reg[p]
		obs.Registry = append(obs.Registry, core.RegistryRecord{Path: p, Entry: e, Alive: g.d.Procs.Alive(e.PID, e.ProcStartTicks())})
	}

	needs := core.Plan(obs)
	obs.ClaudeTranscripts = map[string]core.ClaudeTranscript{}
	for _, n := range needs.Claude {
		p := n.Hint
		if p == "" {
			p = g.findClaude(n.SessionID, n.Cwd, now)
		}
		if p == "" {
			continue
		}
		m, err := g.d.Transcripts.Claude(p)
		if err != nil {
			if !os.IsNotExist(err) {
				obs.Problems = append(obs.Problems, fmt.Sprintf("transcript %s: %v", p, err))
			}
			delete(g.claudePaths, n.SessionID)
			continue
		}
		obs.ClaudeTranscripts[n.SessionID] = core.ClaudeTranscript{Path: p, Meta: m}
	}
	obs.AgyTranscripts = map[string]core.AgyTranscript{}
	for _, n := range needs.Agy {
		p := n.Hint
		if p == "" {
			p = g.d.Files.AgyTranscript(n.ConversationID)
		}
		if m, err := g.d.Transcripts.Agy(p); err == nil {
			obs.AgyTranscripts[n.ConversationID] = core.AgyTranscript{Path: p, Meta: m}
		}
	}
	obs.Projects = map[string][]core.ProjectTranscript{}
	for _, cwd := range needs.Projects {
		files, err := g.d.Files.ListProject(cwd)
		list := []core.ProjectTranscript{} // listed, possibly empty
		if err == nil {
			for _, f := range files {
				if m, err := g.d.Transcripts.Claude(f.Path); err == nil {
					list = append(list, core.ProjectTranscript{SessionID: f.SessionID, Path: f.Path, ModTime: f.ModTime, Meta: m})
				}
			}
		}
		obs.Projects[cwd] = list
	}
	return obs
}

func (g *Gatherer) findClaude(sid, cwd string, now time.Time) string {
	if l, ok := g.claudePaths[sid]; ok && (l.path != "" || now.Sub(l.checked) < missRetry) {
		return l.path
	}
	p := g.d.Files.FindClaude(sid, cwd)
	g.claudePaths[sid] = pathLookup{path: p, checked: now}
	return p
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

// DiskFiles locates transcripts on the real filesystem.
type DiskFiles struct {
	ClaudeProjectsDir string
	AgyBrainDir       string
	// ProjectDir maps a cwd to its project directory.
	ProjectDir func(cwd string) string
}

// FindClaude implements Files.
func (f DiskFiles) FindClaude(sid, cwd string) string {
	if cwd != "" {
		c := filepath.Join(f.ProjectDir(cwd), sid+".jsonl")
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if m, _ := filepath.Glob(filepath.Join(f.ClaudeProjectsDir, "*", sid+".jsonl")); len(m) > 0 {
		return m[0]
	}
	return ""
}

// ListProject implements Files.
func (f DiskFiles) ListProject(cwd string) ([]TranscriptFile, error) {
	dir := f.ProjectDir(cwd)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []TranscriptFile
	for _, d := range ents {
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			continue
		}
		fi, err := d.Info()
		if err != nil {
			continue
		}
		out = append(out, TranscriptFile{SessionID: strings.TrimSuffix(d.Name(), ".jsonl"), Path: filepath.Join(dir, d.Name()), ModTime: fi.ModTime()})
	}
	return out, nil
}

// AgyTranscript implements Files.
func (f DiskFiles) AgyTranscript(id string) string {
	return filepath.Join(f.AgyBrainDir, id, ".system_generated", "logs", "transcript_full.jsonl")
}
