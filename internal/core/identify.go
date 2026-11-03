package core

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"cccp/internal/model"
)

// Key builds a session key.
func Key(a model.Agent, id string) string { return string(a) + ":" + id }

// paneKeyPrefix marks keys of sessions whose id is unknown.
const paneKeyPrefix = "pane:"

// builder accumulates one session's facts from every source.
type builder struct {
	key   string
	agent model.Agent
	sid   string // session id, or paneKeyPrefix+pane when unknown

	hook *HookRecord
	reg  *RegistryRecord
	proc *AgentProc
	conv *Conversation // its transcript in the catalog, if cataloged

	// unmanaged: found only via tmux and /proc; its state is inferred.
	unmanaged bool
	// superseded: another session now runs on this hook session's process.
	superseded bool
	note       string
}

func (b *builder) known() bool { return !strings.HasPrefix(b.sid, paneKeyPrefix) }

func (b *builder) lastEvent() time.Time {
	var t time.Time
	if b.hook != nil {
		t = b.hook.State.LastEventAt
	}
	if b.reg != nil && b.reg.Entry.StatusAt().After(t) {
		t = b.reg.Entry.StatusAt()
	}
	return t
}

// hookLive: the hook state describes a session whose process still runs.
func hookLive(h *HookRecord) bool {
	return h != nil && h.State.Activity != model.Ended && h.ProcAlive
}

// identified is the result of identify.
type identified struct {
	builders map[string]*builder
	needs    Needs
	panes    map[string]model.TmuxPane
	// defaultDir is the default profile's directory.
	defaultDir string
}

// identify decides which sessions exist and which process and pane belongs
// to each. Plan and Compute both use it, so they cannot disagree about which
// transcripts are needed.
func identify(obs Observation) identified {
	id := identified{builders: map[string]*builder{}, panes: map[string]model.TmuxPane{}, defaultDir: defaultDir(obs.Profiles)}
	for _, p := range obs.Tmux.Panes {
		id.panes[p.ID] = p
	}
	get := func(a model.Agent, sid string) *builder {
		k := Key(a, sid)
		if b, ok := id.builders[k]; ok {
			return b
		}
		b := &builder{key: k, agent: a, sid: sid}
		id.builders[k] = b
		return b
	}

	// 1. Hook state files. Print-mode runs are automation, not sessions.
	for i := range obs.Hooks {
		h := &obs.Hooks[i]
		if h.State.SessionID == "" || h.State.Agent == "" || h.State.PrintMode {
			continue
		}
		get(h.State.Agent, h.State.SessionID).hook = h
	}

	// 2. Registry entries of live user sessions. The registry names each
	// process's current session.
	regSIDByPID := map[int]string{}
	for i := range obs.Registry {
		r := &obs.Registry[i]
		if r.Entry.SessionID == "" || !r.Entry.IsUserSession() || !r.Alive {
			continue
		}
		get(model.Claude, r.Entry.SessionID).reg = r
		regSIDByPID[r.Entry.PID] = r.Entry.SessionID
	}

	// 3. One current session per process: the registry's, else the hook
	// session with the latest event. Others on that process are
	// superseded (e.g. /clear without a SessionEnd).
	current := map[int]*builder{}
	for _, b := range sortedBuilders(id.builders) {
		pid := 0
		switch {
		case b.reg != nil:
			pid = b.reg.Entry.PID
		case hookLive(b.hook):
			pid = b.hook.State.PID
		}
		if pid == 0 {
			continue
		}
		if sid, ok := regSIDByPID[pid]; ok && sid != b.sid {
			continue
		}
		if cur, ok := current[pid]; !ok || b.lastEvent().After(cur.lastEvent()) {
			current[pid] = b
		}
	}
	for _, b := range id.builders {
		if b.reg == nil && b.hook != nil && b.hook.State.PID != 0 {
			if cur, ok := current[b.hook.State.PID]; ok && cur != b {
				b.superseded = true
			}
		}
	}

	// 4. Attach pane processes to their sessions.
	agents := paneAgents(obs.Agents)
	for pid, b := range current {
		if ap, ok := agents[pid]; ok {
			b.proc = &ap
		}
	}

	// 5. The remaining agent processes are unmanaged. Newest first: when
	// several processes share a directory, the newest most plausibly owns
	// the newest conversation; each conversation is assigned once.
	var free []AgentProc
	for pid, ap := range agents {
		if _, claimed := current[pid]; !claimed {
			free = append(free, ap)
		}
	}
	sort.Slice(free, func(i, j int) bool {
		if free[i].Start != free[j].Start {
			return free[i].Start > free[j].Start
		}
		return free[i].PID < free[j].PID
	})
	taken := map[string]bool{}
	for k, b := range id.builders {
		if b.reg != nil || (hookLive(b.hook) && !b.superseded) {
			taken[k] = true
		}
	}
	for i := range free {
		ap := free[i]
		sid, note, needProject := resolveUnmanaged(ap, id.panes[ap.Pane], obs, id.defaultDir, taken)
		if needProject {
			id.needs.Projects = appendUniqueKey(id.needs.Projects, ProjectKey{ConfigDir: orDefault(ap.ConfigDir, id.defaultDir), Cwd: ap.Cwd})
		}
		var b *builder
		if sid != "" && !taken[Key(ap.Agent, sid)] {
			b = get(ap.Agent, sid)
			// Hook state from an earlier process of this conversation is
			// stale for this one.
			b.hook = nil
			taken[b.key] = true
		} else {
			if sid != "" {
				note = "session id ambiguous"
			}
			b = get(ap.Agent, paneKeyPrefix+ap.Pane)
		}
		b.proc = &ap
		b.unmanaged = true
		b.note = note
	}

	// 6. Every cataloged conversation: attached to its session, or a
	// closed conversation of its own.
	for i := range obs.Conversations {
		c := &obs.Conversations[i]
		if c.SessionID == "" || c.Agent == "" {
			continue
		}
		get(c.Agent, c.SessionID).conv = c
	}

	// 7. Transcripts needed for known sessions. A closed conversation
	// (no process, no hook state) needs none: the catalog has its facts.
	for _, b := range sortedBuilders(id.builders) {
		if !b.known() || b.closedOnly() {
			continue
		}
		hint := ""
		if b.hook != nil {
			hint = b.hook.State.TranscriptPath
		}
		switch b.agent {
		case model.Claude:
			id.needs.Claude = append(id.needs.Claude, ClaudeNeed{SessionID: b.sid, ConfigDir: orDefault(b.configDir(), id.defaultDir), Cwd: b.cwd(), Hint: hint})
		case model.Agy:
			id.needs.Agy = append(id.needs.Agy, AgyNeed{ConversationID: b.sid, Hint: hint})
		}
	}
	return id
}

// closedOnly: known only from the catalog (no process, hook or registry).
func (b *builder) closedOnly() bool {
	return b.conv != nil && b.hook == nil && b.reg == nil && b.proc == nil
}

// cwd is the session's working directory as reported by its sources.
func (b *builder) cwd() string {
	switch {
	case b.hook != nil && b.hook.State.Cwd != "":
		return b.hook.State.Cwd
	case b.reg != nil && b.reg.Entry.Cwd != "":
		return b.reg.Entry.Cwd
	case b.proc != nil && b.proc.Cwd != "":
		return b.proc.Cwd
	case b.conv != nil:
		return b.conv.Cwd
	}
	return ""
}

// configDir is the session's Claude profile directory as its sources report
// it ("" = the default profile).
func (b *builder) configDir() string {
	switch {
	case b.hook != nil && b.hook.State.ConfigDir != "":
		return b.hook.State.ConfigDir
	case b.reg != nil && b.reg.ConfigDir != "":
		return b.reg.ConfigDir
	case b.proc != nil && b.proc.ConfigDir != "":
		return b.proc.ConfigDir
	case b.conv != nil:
		return b.conv.ConfigDir
	}
	return ""
}

func defaultDir(ps []model.Profile) string {
	for _, p := range ps {
		if p.Default {
			return p.Dir
		}
	}
	return ""
}

func orDefault(dir, def string) string {
	if dir == "" {
		return def
	}
	return dir
}

// profileOf returns the profile for a config directory ("" = default).
func profileOf(ps []model.Profile, dir string) (model.Profile, bool) {
	for _, p := range ps {
		if (dir == "" && p.Default) || (dir != "" && p.Dir == dir) {
			return p, true
		}
	}
	return model.Profile{}, false
}

func appendUniqueKey(xs []ProjectKey, x ProjectKey) []ProjectKey {
	if x.Cwd == "" {
		return xs
	}
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

func sortedBuilders(m map[string]*builder) []*builder {
	out := make([]*builder, 0, len(m))
	for _, b := range m {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

func appendUnique(xs []string, x string) []string {
	if x == "" {
		return xs
	}
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// paneAgents picks, for each pane, the shallowest agent process that is not
// a print-mode run, keyed by pid.
func paneAgents(all []AgentProc) map[int]AgentProc {
	best := map[string]AgentProc{}
	for _, ap := range all {
		if model.IsPrintMode(ap.Cmdline) {
			continue
		}
		cur, ok := best[ap.Pane]
		if !ok || ap.Depth < cur.Depth || (ap.Depth == cur.Depth && ap.PID < cur.PID) {
			best[ap.Pane] = ap
		}
	}
	out := map[int]AgentProc{}
	for _, ap := range best {
		out[ap.PID] = ap
	}
	return out
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// resolveUnmanaged determines the session id of an agent process that no
// hook or registry entry claims. needProject is set when matching needs the
// listing of the process's project directory, which is not in obs yet.
func resolveUnmanaged(ap AgentProc, pane model.TmuxPane, obs Observation, defaultDir string, taken map[string]bool) (sid, note string, needProject bool) {
	switch ap.Agent {
	case model.Claude:
		if s := flagValue(ap.Cmdline, "--resume", "-r", "--session-id"); uuidRe.MatchString(s) {
			return s, "", false
		}
		title := CleanPaneTitle(pane.Title)
		if title == "" {
			return "", "no pane title to match", false
		}
		if ap.Cwd == "" {
			return "", "no working directory", false
		}
		list, listed := obs.Projects[ProjectKey{ConfigDir: orDefault(ap.ConfigDir, defaultDir), Cwd: ap.Cwd}]
		if !listed {
			return "", "", true
		}
		sid, note = matchClaudeTitle(list, title, taken)
		return sid, note, false
	case model.Agy:
		if s := flagValue(ap.Cmdline, "--conversation"); s != "" {
			return s, "", false
		}
		sid, note = matchAgyConversation(obs, ap, taken)
		return sid, note, false
	}
	return "", "", false
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

// CleanPaneTitle strips Claude Code's leading status glyph ("✳ Title").
func CleanPaneTitle(t string) string {
	t = strings.TrimSpace(t)
	r := []rune(t)
	if len(r) > 1 && !unicode.IsLetter(r[0]) && !unicode.IsDigit(r[0]) && unicode.IsSpace(r[1]) {
		return strings.TrimSpace(string(r[2:]))
	}
	return t
}

// matchClaudeTitle finds the project transcript whose ai-title equals the
// pane title; the most recently modified untaken match wins.
func matchClaudeTitle(list []ProjectTranscript, title string, taken map[string]bool) (string, string) {
	var cs []ProjectTranscript
	for _, t := range list {
		if t.Meta.AITitle == title && !taken[Key(model.Claude, t.SessionID)] {
			cs = append(cs, t)
		}
	}
	if len(cs) == 0 {
		return "", "no transcript titled “" + title + "”"
	}
	sort.Slice(cs, func(i, j int) bool {
		if !cs[i].ModTime.Equal(cs[j].ModTime) {
			return cs[i].ModTime.After(cs[j].ModTime)
		}
		return cs[i].SessionID < cs[j].SessionID
	})
	note := ""
	if len(cs) > 1 {
		note = "title matches several transcripts; picked the newest"
	}
	return cs[0].SessionID, note
}

// matchAgyConversation picks the newest untaken conversation in the
// process's directory modified since the process started.
func matchAgyConversation(obs Observation, ap AgentProc, taken map[string]bool) (string, string) {
	best, bestT := "", time.Time{}
	for id, c := range obs.AgyConversations {
		if !containsPath(c.Workspaces, ap.Cwd) || taken[Key(model.Agy, id)] {
			continue
		}
		if !ap.StartedAt.IsZero() && c.LastModified.Before(ap.StartedAt) {
			continue
		}
		if c.LastModified.After(bestT) || (c.LastModified.Equal(bestT) && id < best) {
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

// registryEntry is a convenience for builders with a registry record.
func (b *builder) registryEntry() *model.RegistryEntry {
	if b.reg == nil {
		return nil
	}
	return &b.reg.Entry
}
