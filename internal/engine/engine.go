package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"cccp/internal/agydb"
	"cccp/internal/claudereg"
	"cccp/internal/config"
	"cccp/internal/model"
	"cccp/internal/paths"
	"cccp/internal/power"
	"cccp/internal/procs"
	"cccp/internal/store"
	"cccp/internal/tmux"
	"cccp/internal/transcript"
)

// agentProc is an agent process found in a tmux pane's process tree.
type agentProc struct {
	stat  procs.Stat
	agent model.Agent
	pane  string
}

// resolution caches how an unmanaged process was matched to a session id.
type resolution struct {
	start     uint64
	title     string // pane title the match was made from
	sessionID string
	note      string
}

// Engine holds every source's latest data. It is not safe for concurrent
// use except where noted; the daemon drives it from one goroutine.
type Engine struct {
	cfg     config.Config
	tr      *transcript.Reader
	agydb   *agydb.Cache
	tmux    *tmux.Client
	Persist *Persist
	// daemonStart separates sessions that are new from ones that existed
	// before this engine first looked (see firstSight).
	daemonStart time.Time

	hooks map[string]model.HookState // by state file path
	reg   map[string]claudereg.Entry // by registry file path
	// regPrev is each registry session's last seen status, for transition
	// detection.
	regPrev map[string]string

	panes   []tmux.Pane
	clients []tmux.ClientInfo
	tmuxOK  bool
	tmuxErr string

	agentsInPanes map[int]agentProc // by agent pid
	resolutions   map[int]resolution

	transcriptPaths map[string]transcriptLookup // by session id

	// PersistDirty is set when Persist changed and should be saved.
	PersistDirty bool

	problems []string
}

type transcriptLookup struct {
	path    string
	checked time.Time
}

// New creates an engine. persist may be nil (fresh state).
func New(cfg config.Config, persist *Persist) *Engine {
	if persist == nil {
		persist = newPersist()
	}
	return &Engine{
		cfg:             cfg,
		tr:              transcript.NewReader(),
		agydb:           agydb.NewCache(),
		tmux:            tmux.Default(),
		Persist:         persist,
		daemonStart:     time.Now(),
		hooks:           map[string]model.HookState{},
		reg:             map[string]claudereg.Entry{},
		regPrev:         map[string]string{},
		agentsInPanes:   map[int]agentProc{},
		resolutions:     map[int]resolution{},
		transcriptPaths: map[string]transcriptLookup{},
	}
}

// Tmux returns the engine's tmux client.
func (e *Engine) Tmux() *tmux.Client { return e.tmux }

// Config returns the configuration.
func (e *Engine) Config() config.Config { return e.cfg }

// SetConfig replaces the configuration.
func (e *Engine) SetConfig(c config.Config) { e.cfg = c }

// LoadAll (re)reads every source. Used at startup and on explicit refresh.
func (e *Engine) LoadAll() {
	e.LoadHooks()
	e.LoadRegistry()
	e.RefreshTmux()
	e.Discover()
}

// LoadHooks rereads every hook state file.
func (e *Engine) LoadHooks() {
	all, errs := store.LoadAll()
	e.hooks = all
	e.problems = nil
	for _, err := range errs {
		e.problems = append(e.problems, err.Error())
	}
}

// LoadHookFile rereads one hook state file (or forgets it if gone).
func (e *Engine) LoadHookFile(p string) {
	s, err := store.Load(p)
	if err != nil {
		delete(e.hooks, p)
		return
	}
	e.hooks[p] = s
}

// HookFile returns the loaded state of one hook state file.
func (e *Engine) HookFile(p string) (model.HookState, bool) {
	s, ok := e.hooks[p]
	return s, ok
}

// RegistryFile returns the loaded registry entry of one file.
func (e *Engine) RegistryFile(p string) (claudereg.Entry, bool) {
	r, ok := e.reg[p]
	return r, ok
}

// LoadRegistry rereads every Claude registry file.
func (e *Engine) LoadRegistry() {
	e.reg = map[string]claudereg.Entry{}
	for _, r := range claudereg.ReadAll() {
		e.noteRegistry(r)
		e.reg[r.Path] = r
	}
}

// LoadRegistryFile rereads one registry file (or forgets it if gone).
func (e *Engine) LoadRegistryFile(p string) {
	r, err := claudereg.Read(p)
	if err != nil {
		delete(e.reg, p)
		return
	}
	e.noteRegistry(r)
	e.reg[p] = r
}

// noteRegistry records busy/waiting → idle transitions as completions.
func (e *Engine) noteRegistry(r claudereg.Entry) {
	if r.SessionID == "" {
		return
	}
	key := Key(model.Claude, r.SessionID)
	prev, had := e.regPrev[key]
	e.regPrev[key] = r.Status
	if had && (prev == "busy" || prev == "waiting") && r.Status == "idle" {
		at := r.StatusAt()
		if at.After(e.Persist.RegCompletions[key]) {
			e.Persist.RegCompletions[key] = at
			e.PersistDirty = true
		}
	}
}

// RefreshTmux rereads panes and clients.
func (e *Engine) RefreshTmux() {
	panes, err := e.tmux.ListPanes()
	if err != nil {
		e.panes, e.clients, e.tmuxOK, e.tmuxErr = nil, nil, false, err.Error()
		return
	}
	clients, err := e.tmux.ListClients()
	if err != nil {
		clients = nil
	}
	e.panes, e.clients, e.tmuxOK, e.tmuxErr = panes, clients, true, ""
}

// Discover finds agent processes in tmux panes (one /proc scan).
func (e *Engine) Discover() {
	found := map[int]agentProc{}
	if len(e.panes) > 0 {
		t := procs.Snapshot()
		for _, p := range e.panes {
			st, a, ok := t.FindAgentBelow(p.PID)
			if !ok {
				continue
			}
			found[st.PID] = agentProc{stat: st, agent: model.Agent(a), pane: p.ID}
		}
	}
	e.agentsInPanes = found
	for pid, r := range e.resolutions {
		if ap, ok := found[pid]; !ok || ap.stat.StartTime != r.start {
			delete(e.resolutions, pid)
		}
	}
}

// Key builds a session key.
func Key(a model.Agent, id string) string { return string(a) + ":" + id }

// builder accumulates one session's facts from every source.
type builder struct {
	key   string
	agent model.Agent
	sid   string
	hook  *model.HookState
	reg   *claudereg.Entry
	proc  *agentProc
	// unmanaged: found only via tmux/proc.
	unmanaged bool
	note      string
	// superseded: another session now runs on this hook session's pid.
	superseded bool
}

// Snapshot merges all sources. It may update e.Persist (seen marks).
func (e *Engine) Snapshot(now time.Time) Snapshot {
	snap := Snapshot{At: now, OnBattery: power.OnBattery(), TmuxOK: e.tmuxOK}
	snap.Problems = append(snap.Problems, e.problems...)
	if !e.tmuxOK && e.tmuxErr != "" {
		snap.Problems = append(snap.Problems, "tmux: "+e.tmuxErr)
	}

	paneByID := map[string]tmux.Pane{}
	for _, p := range e.panes {
		paneByID[p.ID] = p
	}
	visibleWindows := map[string]bool{}
	for _, c := range e.clients {
		if !c.Focused {
			continue
		}
		if p, ok := paneByID[c.Pane]; ok {
			visibleWindows[p.WindowID] = true
		}
	}

	b := map[string]*builder{}
	get := func(a model.Agent, sid string) *builder {
		k := Key(a, sid)
		if x, ok := b[k]; ok {
			return x
		}
		x := &builder{key: k, agent: a, sid: sid}
		b[k] = x
		return x
	}

	// 1. Hook state files.
	for p := range e.hooks {
		h := e.hooks[p]
		if h.SessionID == "" || h.Agent == "" {
			continue
		}
		get(h.Agent, h.SessionID).hook = &h
	}
	// 2. Registry entries of live user sessions. The registry names each
	// pid's current session; hook sessions on the same pid with another id
	// have ended (e.g. /clear without a SessionEnd).
	regSIDByPID := map[int]string{}
	for p := range e.reg {
		r := e.reg[p]
		if r.SessionID == "" || !r.IsUserSession() || !r.Alive() {
			continue
		}
		get(model.Claude, r.SessionID).reg = &r
		regSIDByPID[r.PID] = r.SessionID
	}
	// 3. Attach pane processes to known sessions by pid; the rest are
	// unmanaged.
	claimed := map[int]bool{}
	latestByPID := map[int]*builder{}
	for _, x := range b {
		pid := 0
		switch {
		case x.reg != nil:
			pid = x.reg.PID
		case x.hook != nil && x.hook.Activity != model.Ended && procs.Alive(x.hook.PID, x.hook.PIDStart):
			pid = x.hook.PID
		}
		if pid == 0 {
			continue
		}
		if sid, ok := regSIDByPID[pid]; ok && sid != x.sid {
			continue // superseded on this pid
		}
		if cur, ok := latestByPID[pid]; !ok || lastEvent(x).After(lastEvent(cur)) {
			latestByPID[pid] = x
		}
	}
	for _, x := range b {
		if x.hook != nil && x.hook.PID != 0 && latestByPID[x.hook.PID] != x && (x.reg == nil) {
			if _, ok := latestByPID[x.hook.PID]; ok {
				x.superseded = true
			}
		}
	}
	for pid, x := range latestByPID {
		claimed[pid] = true
		if ap, ok := e.agentsInPanes[pid]; ok {
			apc := ap
			x.proc = &apc
		}
	}
	// Unclaimed agent processes, newest first: when several processes share
	// a directory, the newest process most plausibly owns the newest
	// conversation, and each conversation is assigned at most once.
	var free []agentProc
	for pid, ap := range e.agentsInPanes {
		if !claimed[pid] {
			free = append(free, ap)
		}
	}
	sort.Slice(free, func(i, j int) bool { return free[i].stat.StartTime > free[j].stat.StartTime })
	taken := map[string]bool{}
	for k, x := range b {
		if x.reg != nil || (x.hook != nil && x.hook.Activity != model.Ended && procs.Alive(x.hook.PID, x.hook.PIDStart)) {
			taken[k] = true
		}
	}
	for i := range free {
		ap := free[i]
		sid, note := e.resolveUnmanaged(&ap, paneByID[ap.pane], taken)
		var x *builder
		if sid != "" && !taken[Key(ap.agent, sid)] {
			x = get(ap.agent, sid)
			if x.hook != nil {
				// Hook state from an earlier process of the same
				// conversation: stale for this process.
				x.hook = nil
			}
			taken[x.key] = true
		} else {
			if sid != "" {
				note = "session id ambiguous"
			}
			x = get(ap.agent, "pane:"+ap.pane)
		}
		x.proc = &ap
		x.unmanaged = true
		x.note = note
	}

	for _, x := range b {
		s := e.build(x, now, paneByID, visibleWindows)
		if s == nil {
			continue
		}
		snap.Sessions = append(snap.Sessions, *s)
	}
	if !e.Persist.Initialized && (len(e.agentsInPanes) > 0 || len(e.hooks) > 0 || len(e.reg) > 0) {
		e.Persist.Initialized = true
		e.PersistDirty = true
	}
	sortSessions(snap.Sessions, now)
	return snap
}

func lastEvent(x *builder) time.Time {
	t := time.Time{}
	if x.hook != nil {
		t = x.hook.LastEventAt
	}
	if x.reg != nil && x.reg.StatusAt().After(t) {
		t = x.reg.StatusAt()
	}
	return t
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// resolveUnmanaged determines the session id of an agent process that no
// hook or registry entry claims. Results are cached per process and pane
// title.
func (e *Engine) resolveUnmanaged(ap *agentProc, pane tmux.Pane, taken map[string]bool) (string, string) {
	title := cleanPaneTitle(pane.Title)
	// Only Claude matches are cached (keyed by pane title, which changes when
	// the conversation does). agy matches depend on its summary database and
	// are cheap to recompute, so they are not cached.
	if r, ok := e.resolutions[ap.stat.PID]; ok && ap.agent == model.Claude && r.start == ap.stat.StartTime && r.title == title {
		return r.sessionID, r.note
	}
	sid, note := "", ""
	args := procs.Cmdline(ap.stat.PID)
	switch ap.agent {
	case model.Claude:
		sid = flagValue(args, "--resume", "-r", "--session-id")
		if sid != "" && !uuidRe.MatchString(sid) {
			sid = ""
		}
		if sid == "" && title != "" {
			sid, note = e.matchClaudeTitle(procs.Cwd(ap.stat.PID), title, taken)
		}
	case model.Agy:
		sid = flagValue(args, "--conversation")
		if sid == "" {
			sid, note = e.matchAgyConversation(procs.Cwd(ap.stat.PID), ap.stat.StartTime, taken)
		}
	}
	e.resolutions[ap.stat.PID] = resolution{start: ap.stat.StartTime, title: title, sessionID: sid, note: note}
	return sid, note
}

func flagValue(args []string, names ...string) string {
	for i, a := range args {
		for _, n := range names {
			if a == n && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1]
			}
			if strings.HasPrefix(a, n+"=") {
				return strings.TrimPrefix(a, n+"=")
			}
		}
	}
	return ""
}

// cleanPaneTitle strips Claude's leading status glyph ("✳ Title").
func cleanPaneTitle(t string) string {
	t = strings.TrimSpace(t)
	r := []rune(t)
	if len(r) > 1 && !unicode.IsLetter(r[0]) && !unicode.IsDigit(r[0]) && unicode.IsSpace(r[1]) {
		return strings.TrimSpace(string(r[2:]))
	}
	return t
}

// matchClaudeTitle finds the transcript in cwd's project directory whose
// ai-title equals the pane title. The most recently modified match wins.
func (e *Engine) matchClaudeTitle(cwd, title string, taken map[string]bool) (string, string) {
	if cwd == "" {
		return "", "no cwd"
	}
	dir := paths.ClaudeProjectDir(cwd)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", "no transcripts for " + cwd
	}
	type cand struct {
		sid string
		mod time.Time
	}
	var cs []cand
	for _, d := range ents {
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			continue
		}
		sid := strings.TrimSuffix(d.Name(), ".jsonl")
		if taken[Key(model.Claude, sid)] {
			continue // tracked elsewhere
		}
		p := filepath.Join(dir, d.Name())
		m, err := e.tr.Claude(p)
		if err != nil || m.AITitle != title {
			continue
		}
		fi, err := d.Info()
		if err != nil {
			continue
		}
		cs = append(cs, cand{sid, fi.ModTime()})
	}
	if len(cs) == 0 {
		return "", "no transcript titled “" + title + "”"
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].mod.After(cs[j].mod) })
	note := ""
	if len(cs) > 1 {
		note = "title matches several transcripts; picked the newest"
	}
	return cs[0].sid, note
}

// matchAgyConversation picks the newest conversation in cwd modified since
// the process started.
func (e *Engine) matchAgyConversation(cwd string, startTicks uint64, taken map[string]bool) (string, string) {
	all := e.agydb.All()
	started := procs.StartWall(startTicks)
	best, bestT := "", time.Time{}
	for id, c := range all {
		if !containsPath(c.Workspaces, cwd) || taken[Key(model.Agy, id)] {
			continue
		}
		if !started.IsZero() && c.LastModified.Before(started) {
			continue
		}
		if c.LastModified.After(bestT) {
			best, bestT = id, c.LastModified
		}
	}
	if best == "" {
		return "", "no conversation since agy started"
	}
	return best, "conversation matched by workspace and time"
}

func containsPath(list []string, p string) bool {
	for _, x := range list {
		if x == p {
			return true
		}
	}
	return false
}

// claudeTranscript finds a Claude transcript by session id.
func (e *Engine) claudeTranscript(sid, cwd, hinted string, now time.Time) string {
	if hinted != "" {
		return hinted
	}
	if l, ok := e.transcriptPaths[sid]; ok && (l.path != "" || now.Sub(l.checked) < time.Minute) {
		return l.path
	}
	p := ""
	if cwd != "" {
		c := filepath.Join(paths.ClaudeProjectDir(cwd), sid+".jsonl")
		if _, err := os.Stat(c); err == nil {
			p = c
		}
	}
	if p == "" {
		if m, _ := filepath.Glob(filepath.Join(paths.ClaudeProjectsDir(), "*", sid+".jsonl")); len(m) > 0 {
			p = m[0]
		}
	}
	e.transcriptPaths[sid] = transcriptLookup{path: p, checked: now}
	return p
}

func agyTranscript(sid, hinted string) string {
	if hinted != "" {
		return hinted
	}
	return filepath.Join(paths.AgyDir(), "brain", sid, ".system_generated", "logs", "transcript_full.jsonl")
}

// NextDeadline returns the earliest future time at which a snapshot would
// change without any event (a cache expiring), or zero.
func NextDeadline(s Snapshot) time.Time {
	var t time.Time
	for _, x := range s.Sessions {
		if x.Group == GroupWorking || x.Group == GroupEnded || !x.Cache.Known {
			continue
		}
		if x.Cache.ExpiresAt.After(s.At) && (t.IsZero() || x.Cache.ExpiresAt.Before(t)) {
			t = x.Cache.ExpiresAt
		}
	}
	return t
}

// PruneEnded deletes hook state files of sessions that ended longer ago
// than the retention period, and drops their persisted records.
func (e *Engine) PruneEnded(s Snapshot) int {
	n := 0
	keep := map[string]bool{}
	for _, x := range s.Sessions {
		keep[x.Key] = true
	}
	for p, h := range e.hooks {
		end := h.EndedAt
		if end.IsZero() {
			end = h.LastEventAt
		}
		k := Key(h.Agent, h.SessionID)
		ended := h.Activity == model.Ended || !procs.Alive(h.PID, h.PIDStart)
		if ended && s.At.Sub(end) > e.cfg.EndedRetention.Duration {
			if store.Remove(p) == nil {
				delete(e.hooks, p)
				delete(keep, k)
				n++
			}
		}
	}
	before := len(e.Persist.Seen) + len(e.Persist.RegCompletions) + len(e.Persist.Summaries)
	e.Persist.Prune(keep)
	if len(e.Persist.Seen)+len(e.Persist.RegCompletions)+len(e.Persist.Summaries) != before {
		e.PersistDirty = true
	}
	return n
}
