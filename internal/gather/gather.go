// Package gather is the read side of the imperative shell: it turns the
// world (hook state files, Claude's registries, tmux, /proc, transcripts,
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

	"cccp/internal/catalog"
	"cccp/internal/claudereg"
	"cccp/internal/config"
	"cccp/internal/core"
	"cccp/internal/model"
	"cccp/internal/paths"
	"cccp/internal/procs"
	"cccp/internal/profile"
	"cccp/internal/tmux"
	"cccp/internal/transcript"
)

// HookStore reads hook state files.
type HookStore interface {
	LoadAll() (map[string]model.HookState, []error)
	Load(path string) (model.HookState, error)
}

// Registry reads a Claude Code session registry directory.
type Registry interface {
	ReadAll(dir string) []claudereg.Entry
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
	Environ(pid int, key string) string
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

// Files locates transcripts and Claude profiles on disk.
type Files interface {
	// FindClaude returns a Claude session's transcript within a profile,
	// looking in cwd's project directory first; "" when there is none.
	FindClaude(sessionID, configDir, cwd string) string
	// ListProject lists the transcripts of cwd's project in a profile.
	ListProject(configDir, cwd string) ([]TranscriptFile, error)
	// AgyTranscript is the transcript path of an agy conversation.
	AgyTranscript(conversationID string) string
	// ProfileDirs lists directories that look like Claude profiles
	// (~/.claude*, containing sessions/ or projects/).
	ProfileDirs() []string
}

// AgyDB reads agy's conversation summaries.
type AgyDB interface {
	All() map[string]model.AgyConversation
}

// Power reports the power source.
type Power interface {
	OnBattery() bool
}

// Catalog lists the cataloged conversations (catalog.Index).
type Catalog interface {
	Docs() ([]catalog.Doc, error)
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
	Catalog     Catalog
	Hostname    string

	// Home, DefaultClaudeDir and Configured feed profile.Resolve.
	Home             string
	DefaultClaudeDir string
	Configured       []config.Profile
}

// Gatherer holds the last view of every source.
type Gatherer struct {
	d Deps

	hooks    map[string]model.HookState // by path
	hookErrs []string
	reg      map[string]claudereg.Entry // by path
	tmux     core.TmuxView
	agents   []core.AgentProc
	// conversations is the catalog as of the last RefreshCatalog.
	conversations []core.Conversation
	catalogErr    string

	// profiles is resolved from configuration and every config directory
	// seen (discovered); registries are read for each.
	profiles   []model.Profile
	discovered map[string]bool

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
	g := &Gatherer{d: d, hooks: map[string]model.HookState{}, reg: map[string]claudereg.Entry{},
		discovered: map[string]bool{}, claudePaths: map[string]pathLookup{}}
	g.resolveProfiles()
	return g
}

// Profiles returns the known Claude profiles (the default first).
func (g *Gatherer) Profiles() []model.Profile { return append([]model.Profile(nil), g.profiles...) }

func (g *Gatherer) resolveProfiles() {
	var disc []string
	for d := range g.discovered {
		disc = append(disc, d)
	}
	g.profiles = profile.Resolve(g.d.Configured, g.d.DefaultClaudeDir, g.d.Home, disc)
}

// noteConfigDir records a config directory seen on a session. When it is a
// new profile, the profile list is re-resolved and its registry read.
func (g *Gatherer) noteConfigDir(dir string) bool {
	dir = profile.Normalize(dir, g.d.Home)
	if dir == "" {
		return false
	}
	if _, known := profile.Find(g.profiles, dir, g.d.Home); known {
		return false
	}
	g.discovered[dir] = true
	g.resolveProfiles()
	if p, ok := profile.Find(g.profiles, dir, g.d.Home); ok {
		g.readRegistry(p)
	}
	return true
}

// Reload rereads every hook state file and every profile's registry, and
// rescans for profile directories.
func (g *Gatherer) Reload() {
	for _, d := range g.d.Files.ProfileDirs() {
		g.discovered[profile.Normalize(d, g.d.Home)] = true
	}
	all, errs := g.d.Hooks.LoadAll()
	g.hooks = all
	g.hookErrs = g.hookErrs[:0]
	for _, err := range errs {
		g.hookErrs = append(g.hookErrs, err.Error())
	}
	for _, st := range g.hooks {
		if d := profile.Normalize(st.ConfigDir, g.d.Home); d != "" {
			g.discovered[d] = true
		}
	}
	g.resolveProfiles()
	g.reg = map[string]claudereg.Entry{}
	for _, p := range g.profiles {
		g.readRegistry(p)
	}
}

func (g *Gatherer) readRegistry(p model.Profile) {
	for _, e := range g.d.Registry.ReadAll(p.RegistryDir()) {
		g.reg[e.Path] = e
	}
}

// HookFileChanged rereads one hook state file.
func (g *Gatherer) HookFileChanged(path string) core.HookChange {
	before, had := g.hooks[path]
	after, err := g.d.Hooks.Load(path)
	has := err == nil
	if has {
		g.hooks[path] = after
		g.noteConfigDir(after.ConfigDir)
	} else {
		delete(g.hooks, path)
	}
	return core.HookChange{
		Rediscover: had != has || before.PID != after.PID || before.Pane != after.Pane,
		Ended:      has && after.Activity == model.Ended && (!had || before.Activity != model.Ended),
	}
}

// RefreshCatalog rereads the cataloged conversations (after the index was
// updated).
func (g *Gatherer) RefreshCatalog() {
	docs, err := g.d.Catalog.Docs()
	if err != nil {
		g.catalogErr = "catalog: " + err.Error()
		return
	}
	g.catalogErr = ""
	g.conversations = g.conversations[:0]
	for _, d := range docs {
		g.conversations = append(g.conversations, core.Conversation{
			Agent: d.Agent, SessionID: d.SessionID, ConfigDir: profile.Normalize(d.ConfigDir, g.d.Home),
			Path: d.Source, Cwd: d.Cwd, Title: d.Title, FirstPrompt: d.FirstPrompt, LastPrompt: d.LastPrompt,
			LastMessage: d.LastMessage, Branch: d.Branch, Model: d.Model, Turns: d.Turns, First: d.First, Last: d.Last,
		})
	}
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

// Discover scans /proc for agent processes under the known panes. A Claude
// process's CLAUDE_CONFIG_DIR names its profile; a new profile's registry is
// read at once.
func (g *Gatherer) Discover() {
	g.agents = nil
	if len(g.tmux.Panes) == 0 {
		return
	}
	t := g.d.Procs.Snapshot()
	for _, p := range g.tmux.Panes {
		for _, f := range t.AgentsBelow(p.PID) {
			ap := core.AgentProc{
				PID:       f.Stat.PID,
				Start:     f.Stat.StartTime,
				StartedAt: g.d.Procs.StartWall(f.Stat.StartTime),
				Agent:     model.Agent(f.Agent),
				Pane:      p.ID,
				Depth:     f.Depth,
				Cmdline:   g.d.Procs.Cmdline(f.Stat.PID),
				Cwd:       g.d.Procs.Cwd(f.Stat.PID),
			}
			if ap.Agent == model.Claude {
				ap.ConfigDir = profile.Normalize(g.d.Procs.Environ(f.Stat.PID, "CLAUDE_CONFIG_DIR"), g.d.Home)
				g.noteConfigDir(ap.ConfigDir)
			}
			g.agents = append(g.agents, ap)
		}
	}
}

// profileOfRegistry is the profile whose registry directory holds path.
func (g *Gatherer) profileOfRegistry(path string) (model.Profile, bool) {
	dir := filepath.Dir(path)
	for _, p := range g.profiles {
		if p.RegistryDir() == dir {
			return p, true
		}
	}
	return model.Profile{}, false
}

// Observe assembles an observation from the current view, reading the
// transcripts core.Plan asks for.
func (g *Gatherer) Observe(now time.Time) core.Observation {
	obs := core.Observation{
		Tmux:             g.tmux,
		Agents:           g.agents,
		OnBattery:        g.d.Power.OnBattery(),
		Hostname:         g.d.Hostname,
		Profiles:         g.Profiles(),
		AgyConversations: g.d.AgyDB.All(),
		Conversations:    append([]core.Conversation(nil), g.conversations...),
		Problems:         append([]string(nil), g.hookErrs...),
	}
	if g.catalogErr != "" {
		obs.Problems = append(obs.Problems, g.catalogErr)
	}
	for _, p := range sortedKeys(g.hooks) {
		st := g.hooks[p]
		st.ConfigDir = profile.Normalize(st.ConfigDir, g.d.Home)
		obs.Hooks = append(obs.Hooks, core.HookRecord{Path: p, State: st, ProcAlive: st.PID != 0 && g.d.Procs.Alive(st.PID, st.PIDStart)})
	}
	for _, p := range sortedKeys(g.reg) {
		e := g.reg[p]
		rec := core.RegistryRecord{Path: p, Entry: e, Alive: g.d.Procs.Alive(e.PID, e.ProcStartTicks())}
		if prof, ok := g.profileOfRegistry(p); ok {
			rec.ConfigDir = prof.Dir
		}
		obs.Registry = append(obs.Registry, rec)
	}

	needs := core.Plan(obs)
	obs.ClaudeTranscripts = map[string]core.ClaudeTranscript{}
	for _, n := range needs.Claude {
		p := n.Hint
		if p == "" {
			p = g.findClaude(n, now)
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
	obs.Projects = map[core.ProjectKey][]core.ProjectTranscript{}
	for _, k := range needs.Projects {
		files, err := g.d.Files.ListProject(k.ConfigDir, k.Cwd)
		list := []core.ProjectTranscript{} // listed, possibly empty
		if err == nil {
			for _, f := range files {
				if m, err := g.d.Transcripts.Claude(f.Path); err == nil {
					list = append(list, core.ProjectTranscript{SessionID: f.SessionID, Path: f.Path, ModTime: f.ModTime, Meta: m})
				}
			}
		}
		obs.Projects[k] = list
	}
	return obs
}

func (g *Gatherer) findClaude(n core.ClaudeNeed, now time.Time) string {
	if l, ok := g.claudePaths[n.SessionID]; ok && (l.path != "" || now.Sub(l.checked) < missRetry) {
		return l.path
	}
	p := g.d.Files.FindClaude(n.SessionID, n.ConfigDir, n.Cwd)
	g.claudePaths[n.SessionID] = pathLookup{path: p, checked: now}
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

// DiskFiles locates transcripts and profiles on the real filesystem.
type DiskFiles struct {
	Home        string
	AgyBrainDir string
}

// FindClaude implements Files.
func (f DiskFiles) FindClaude(sid, configDir, cwd string) string {
	if cwd != "" {
		c := filepath.Join(paths.ClaudeProjectDir(configDir, cwd), sid+".jsonl")
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if m, _ := filepath.Glob(filepath.Join(configDir, "projects", "*", sid+".jsonl")); len(m) > 0 {
		return m[0]
	}
	return ""
}

// ListProject implements Files.
func (f DiskFiles) ListProject(configDir, cwd string) ([]TranscriptFile, error) {
	dir := paths.ClaudeProjectDir(configDir, cwd)
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

// ProfileDirs implements Files: directories ~/.claude* that contain a
// sessions/ or projects/ directory.
func (f DiskFiles) ProfileDirs() []string {
	ents, err := os.ReadDir(f.Home)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".claude") {
			continue
		}
		dir := filepath.Join(f.Home, e.Name())
		for _, sub := range []string{"sessions", "projects"} {
			if fi, err := os.Stat(filepath.Join(dir, sub)); err == nil && fi.IsDir() {
				out = append(out, dir)
				break
			}
		}
	}
	return out
}
