package core

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cccp/internal/config"
	"cccp/internal/model"
)

// class is the coarse state both hooks and the registry can express.
type class int

const (
	cWorking class = iota
	cWaiting
	cIdle
	cEnded
	cUnknown
)

func classOf(a model.Activity) class {
	switch a {
	case model.Working:
		return cWorking
	case model.NeedsApproval, model.NeedsInput:
		return cWaiting
	case model.Idle, model.Errored:
		return cIdle
	case model.Ended:
		return cEnded
	}
	return cUnknown
}

func maxTime(ts ...time.Time) time.Time {
	var m time.Time
	for _, t := range ts {
		if t.After(m) {
			m = t
		}
	}
	return m
}

// computation carries the inputs every per-session rule reads, and the
// memory being updated.
type computation struct {
	obs        Observation
	id         identified
	visible    map[string]bool // window id → visible on a focused client
	cfg        config.Config
	watchStart time.Time
	now        time.Time
	mem        Memory // a private clone; build updates it
}

// meta is a session's transcript facts, when known.
type meta struct {
	claude    model.ClaudeMeta
	agy       model.AgyMeta
	hasClaude bool
	hasAgy    bool
	path      string
}

func (c *computation) transcriptFor(b *builder) meta {
	var m meta
	if !b.known() {
		return m
	}
	switch b.agent {
	case model.Claude:
		if t, ok := c.obs.ClaudeTranscripts[b.sid]; ok && t.Path != "" {
			return meta{claude: t.Meta, hasClaude: true, path: t.Path}
		}
		// Matched by title from a project listing.
		for _, t := range c.obs.Projects[ProjectKey{ConfigDir: orDefault(b.configDir(), c.id.defaultDir), Cwd: b.cwd()}] {
			if t.SessionID == b.sid {
				return meta{claude: t.Meta, hasClaude: true, path: t.Path}
			}
		}
	case model.Agy:
		if t, ok := c.obs.AgyTranscripts[b.sid]; ok && t.Path != "" {
			return meta{agy: t.Meta, hasAgy: true, path: t.Path}
		}
	}
	return m
}

// build derives one session. It returns false for sessions that ended
// longer ago than the retention period.
func (c *computation) build(b *builder) (Session, bool) {
	h, r := b.hook, b.registryEntry()
	var hs *model.HookState
	if h != nil {
		hs = &h.State
	}
	s := Session{Key: b.key, Agent: b.agent}
	if b.known() {
		s.SessionID = b.sid
	}
	switch {
	case b.closedOnly():
		s.Tracking = TrackCatalog
	case b.unmanaged:
		s.Tracking = TrackUnmanaged
	case h != nil && r != nil:
		s.Tracking = TrackHooksRegistry
	case r != nil:
		s.Tracking = TrackRegistry
	default:
		s.Tracking = TrackHooks
	}
	if b.note != "" {
		s.Notes = append(s.Notes, b.note)
	}

	// Liveness and process.
	switch {
	case r != nil:
		s.Alive, s.PID, s.Kind = true, r.PID, r.Kind
	case b.proc != nil:
		s.Alive, s.PID = true, b.proc.PID
	case h != nil:
		s.PID = hs.PID
		if hs.PID == 0 {
			// The hook could not identify the agent process; fall back to
			// whether its pane still runs an agent.
			s.Alive = hs.Activity != model.Ended && c.ourServer(hs.TmuxSocket) && c.paneHasAgent(hs.Pane)
		} else {
			s.Alive = hookLive(h) && !b.superseded
		}
	}
	if s.Kind == "" {
		s.Kind = "interactive"
	}

	// Pane: the process tree is authoritative (it is this server's), then
	// the registry, then the hook, each only if recorded on this server.
	pane := ""
	switch {
	case b.proc != nil && s.Alive:
		pane = b.proc.Pane
	case r != nil && r.Pane() != "" && c.ourServer(b.reg.TmuxSocket):
		pane = r.Pane()
	case hs != nil && s.Alive && c.ourServer(hs.TmuxSocket):
		pane = hs.Pane
	}
	var tp model.TmuxPane
	if p, ok := c.id.panes[pane]; ok && s.Alive {
		tp = p
		s.Pane, s.WindowID, s.Location, s.TmuxSess = p.ID, p.WindowID, p.Location(), p.Session
		s.Visible = c.visible[p.WindowID]
	} else if s.Kind == "background" {
		s.Location = "background"
	}
	s.Cwd = b.cwd()
	if b.agent == model.Claude {
		s.ConfigDir = orDefault(b.configDir(), c.id.defaultDir)
		s.DefaultProfile = s.ConfigDir == c.id.defaultDir

		if p, ok := profileOf(c.obs.Profiles, s.ConfigDir); ok {
			s.Profile = p.Name
		}
	}

	m := c.transcriptFor(b)
	s.TranscriptPath = m.path
	if s.TranscriptPath == "" && b.conv != nil {
		s.TranscriptPath = b.conv.Path
	}

	// Activity.
	switch {
	case !s.Alive:
		s.Activity = model.Ended
		if hs != nil {
			s.ActivitySince = hs.EndedAt
			if s.ActivitySince.IsZero() {
				s.ActivitySince = hs.LastEventAt
			}
		}
		if b.conv != nil && b.conv.Last.After(s.ActivitySince) {
			s.ActivitySince = b.conv.Last
		}
	case b.unmanaged:
		infer(&s, b.agent, m, tp, c.cfg.QuietAfter.Duration, c.now)
	default:
		mergeActivity(&s, hs, r, m.claude)
		s.BackgroundShell = r != nil && r.BackgroundShell() && s.Activity == model.Idle
	}
	if hs != nil {
		s.LastEventAt = hs.LastEventAt
		if s.Error == "" {
			s.Error = hs.Error
		}
	}
	if r != nil && r.StatusAt().After(s.LastEventAt) {
		s.LastEventAt = r.StatusAt()
	}
	if b.conv != nil && b.conv.Last.After(s.LastEventAt) {
		s.LastEventAt = b.conv.Last
	}

	// Times.
	switch {
	case hs != nil && !hs.StartedAt.IsZero():
		s.StartedAt = hs.StartedAt
	case r != nil:
		s.StartedAt = r.StartedAt()
	case b.proc != nil:
		s.StartedAt = b.proc.StartedAt
	case b.conv != nil:
		s.StartedAt = b.conv.First
	}
	if hs != nil {
		s.EndedAt = hs.EndedAt
	}
	if !s.Alive && s.EndedAt.IsZero() {
		s.EndedAt = s.LastEventAt
	}

	c.fillText(&s, hs, r, m, tp, b.conv)

	// Model and context.
	if b.conv != nil {
		s.Model, s.Branch, s.Turns = b.conv.Model, b.conv.Branch, b.conv.Turns
	}
	if hs != nil && hs.Model != "" {
		s.Model = hs.Model
	}
	if m.hasClaude {
		if m.claude.Model != "" {
			s.Model = m.claude.Model
		}
		s.ContextTokens = m.claude.ContextTokens
	} else if m.hasAgy {
		s.ContextTokens = m.agy.ContextTokens
	}

	// Prompt cache.
	switch {
	case b.agent == model.Claude && m.hasClaude && !m.claude.LastActivity().IsZero():
		ttl, assumed := m.claude.TTL, false
		if ttl == 0 {
			ttl, assumed = c.cfg.DefaultCacheTTL.Duration, true
		}
		last := m.claude.LastActivity()
		s.Cache = Cache{Known: true, TTL: ttl, TTLAssumed: assumed, LastActivity: last, ExpiresAt: last.Add(ttl)}
	case b.agent == model.Agy && m.hasAgy && !m.agy.LastModelCallAt.IsZero():
		last := m.agy.LastModelCallAt
		s.Cache = Cache{Known: true, Fades: true, TTL: AgyColdAfter, LastActivity: last,
			CoolingAt: last.Add(AgyWarmFor), ExpiresAt: last.Add(AgyColdAfter)}
	}

	c.attention(&s, b, hs, m)

	s.Alias = c.mem.Aliases[s.Key]
	s.Group = groupOf(s, c.now)
	// A summary describes the state it was written for: a newer turn's
	// session shows none until its own is written.
	if rec, ok := c.mem.Summaries[s.Key]; ok {
		if _, hash := SummaryInput(s); rec.InputHash == hash {
			s.Summary, s.SummaryVerdict = rec.Text, rec.Verdict
		}
	}
	// A finished turn waiting on its own background shell, with nothing
	// for the user yet, is still work in progress.
	if s.Group == GroupYourTurn && s.BackgroundShell && s.SummaryVerdict == VerdictWait {
		s.Group = GroupWorking
	}
	// A closed session is listed while its transcript exists; without one
	// (only hook state left), for the retention period.
	if s.Group == GroupClosed && b.conv == nil && !s.EndedAt.IsZero() && c.now.Sub(s.EndedAt) > c.cfg.EndedRetention.Duration {
		return Session{}, false
	}
	return s, true
}

// ourServer: a pane id recorded on server sock can be looked up here.
func (c *computation) ourServer(sock string) bool {
	return model.SameTmuxServer(sock, c.obs.Tmux.Socket)
}

func (c *computation) paneHasAgent(pane string) bool {
	if pane == "" {
		return false
	}
	if _, ok := c.id.panes[pane]; !ok {
		return false
	}
	for _, ap := range c.obs.Agents {
		if ap.Pane == pane {
			return true
		}
	}
	return false
}

// mergeActivity applies "latest observation wins" between hooks and the
// registry. When both agree on the coarse class, the hook's finer activity
// and detail are kept.
func mergeActivity(s *Session, h *model.HookState, r *model.RegistryEntry, cm model.ClaudeMeta) {
	if h == nil && r == nil {
		s.Activity = model.Unknown
		return
	}
	var regAct model.Activity
	var regAt time.Time
	if r != nil {
		regAct, regAt = r.Activity(), r.StatusAt()
	}
	// A registry status cccp does not know says nothing: it never
	// overrides the hook's (Claude Code adds statuses over time).
	useHook := h != nil && (r == nil || regAct == model.Unknown || classOf(h.Activity) == classOf(regAct) || !regAt.After(h.ActivityAt))
	if useHook {
		s.Activity, s.ActivitySince = h.Activity, h.ActivityAt
		s.Tool, s.ToolDetail, s.Question = h.Tool, h.ToolDetail, h.Question
		if r != nil && classOf(h.Activity) == classOf(regAct) && !regAt.IsZero() && regAt.Before(h.ActivityAt) {
			// The registry saw this class first (e.g. busy before the
			// first hook of the turn): the activity began then.
			s.ActivitySince = regAt
		}
		if s.Question == "" && r != nil {
			s.Question = r.Question()
		}
		return
	}
	s.Activity, s.ActivitySince = regAct, regAt
	s.Question = r.Question()
	if regAct == model.NeedsApproval && cm.PendingToolName != "" {
		s.Tool = cm.PendingToolName
	}
}

// infer classifies a session that no hook or registry observes.
func infer(s *Session, a model.Agent, m meta, p model.TmuxPane, quiet time.Duration, now time.Time) {
	s.Inferred = true
	// Output age is only meaningful for single-pane windows: tmux tracks
	// activity per window.
	outAge, outKnown := time.Duration(0), false
	if p.WindowPanes == 1 && !p.WindowActivity.IsZero() {
		outAge, outKnown = now.Sub(p.WindowActivity), true
	}
	cm, am := m.claude, m.agy
	switch a {
	case model.Claude:
		switch {
		case m.hasClaude && cm.PendingTool:
			s.Tool = cm.PendingToolName
			if outKnown && outAge >= quiet {
				// A running tool keeps Claude's spinner (and so the pane)
				// updating; a pending tool with a silent pane is waiting for
				// permission.
				s.Activity, s.ActivitySince = model.NeedsApproval, maxTime(cm.LastAssistantAt, p.WindowActivity)
				s.Question = "tool call pending and the pane is quiet: probably waiting for approval"
			} else {
				s.Activity, s.ActivitySince = model.Working, cm.LastAssistantAt
			}
		case m.hasClaude && cm.LastUserAt.After(cm.LastAssistantAt) && !cm.Interrupted:
			if outKnown && outAge >= time.Minute {
				s.Activity, s.ActivitySince = model.Unknown, cm.LastUserAt
				s.Notes = append(s.Notes, "prompt sent, no response yet, pane quiet")
			} else {
				s.Activity, s.ActivitySince = model.Working, cm.LastUserAt
			}
		case m.hasClaude && !cm.LastAssistantAt.IsZero():
			s.Activity, s.ActivitySince = model.Idle, maxTime(cm.LastAssistantAt, cm.LastUserAt)
		case outKnown && outAge < quiet:
			s.Activity, s.ActivitySince = model.Working, p.WindowActivity
			s.Notes = append(s.Notes, "no transcript; pane is producing output")
		default:
			s.Activity, s.ActivitySince = model.Unknown, p.WindowActivity
			s.Notes = append(s.Notes, "no transcript found")
		}
	case model.Agy:
		// agy redraws its pane continuously even when idle, so pane output
		// says nothing; only the transcript is used.
		switch {
		case m.hasAgy && am.LastStepType == "PLANNER_RESPONSE" && !am.PendingTools && am.LastStepStatus == "DONE":
			s.Activity, s.ActivitySince = model.Idle, am.LastStepAt
		case m.hasAgy && am.LastStepStatus != "" && am.LastStepStatus != "DONE":
			s.Activity, s.ActivitySince = model.Working, am.LastStepAt
		default:
			s.Activity = model.Unknown
			if m.hasAgy {
				s.ActivitySince = am.LastStepAt
			}
		}
	}
}

// fillText chooses the first and last prompt, last message and title.
func (c *computation) fillText(s *Session, h *model.HookState, r *model.RegistryEntry, m meta, p model.TmuxPane, conv *Conversation) {
	// Last prompt / message: whichever source is newest.
	var msgAt time.Time
	if h != nil {
		// State written before automatic prompts were skipped may still
		// hold one: it is not the user's prompt.
		if !model.AutoPrompt(h.LastPrompt) {
			s.LastPrompt, s.LastPromptAt = h.LastPrompt, h.LastPromptAt
		}
		s.LastMessage, msgAt = h.LastMessage, h.LastMessageAt
	}
	if m.hasClaude {
		cm := m.claude
		s.AutoTurn = cm.LastTurnAuto
		if cm.LastPrompt != "" && (s.LastPrompt == "" || cm.LastPromptAt.After(s.LastPromptAt)) {
			s.LastPrompt = cm.LastPrompt
			if !cm.LastPromptAt.IsZero() {
				s.LastPromptAt = cm.LastPromptAt
			}
		}
		// The Stop hook's message is exact; the transcript wins only when
		// clearly newer (the transcript is written asynchronously).
		if cm.LastAssistantText != "" && (s.LastMessage == "" || cm.LastAssistantAt.After(msgAt.Add(2*time.Second))) {
			s.LastMessage = cm.LastAssistantText
		}
	}
	if m.hasAgy {
		am := m.agy
		if am.LastPrompt != "" && (s.LastPrompt == "" || am.LastPromptAt.After(s.LastPromptAt)) {
			s.LastPrompt, s.LastPromptAt = am.LastPrompt, am.LastPromptAt
		}
		if am.LastMessage != "" {
			s.LastMessage = am.LastMessage
		}
	}
	if conv != nil {
		s.FirstPrompt = conv.FirstPrompt
		if s.LastPrompt == "" {
			s.LastPrompt = conv.LastPrompt
		}
		if s.LastMessage == "" {
			s.LastMessage = conv.LastMessage
		}
	}

	type cand struct{ v, src string }
	var cs []cand
	if h != nil {
		cs = append(cs, cand{h.CustomTitle, "custom"})
	}
	if r != nil && r.NameSource != "" && r.NameSource != "derived" {
		cs = append(cs, cand{r.Name, "name"})
	}
	if m.hasClaude {
		cs = append(cs, cand{m.claude.AITitle, "ai-title"})
	}
	if conv != nil && s.Agent == model.Claude {
		cs = append(cs, cand{conv.Title, "ai-title"})
	}
	if s.Agent == model.Agy && s.SessionID != "" {
		if conv, ok := c.obs.AgyConversations[s.SessionID]; ok {
			cs = append(cs, cand{conv.Title, "agy"}, cand{firstLine(conv.Preview), "agy-preview"})
		}
		if m.hasAgy {
			cs = append(cs, cand{firstLine(m.agy.LastPrompt), "prompt"})
		}
	}
	// tmux's default pane title is the host name, which says nothing.
	host := c.obs.Hostname
	if pt := CleanPaneTitle(p.Title); pt != "" && (host == "" || (pt != host && !strings.HasPrefix(pt, host+"."))) {
		cs = append(cs, cand{pt, "pane"})
	}
	if r != nil {
		cs = append(cs, cand{r.Name, "name"})
	}
	if conv != nil {
		cs = append(cs, cand{conv.Title, "catalog"}, cand{firstLine(conv.FirstPrompt), "prompt"})
	}
	if s.Cwd != "" {
		cs = append(cs, cand{filepath.Base(s.Cwd), "cwd"})
	}
	for _, x := range cs {
		if v := strings.TrimSpace(x.v); v != "" {
			s.Title, s.TitleSource = v, x.src
			return
		}
	}
	s.Title, s.TitleSource = "(untitled)", ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// attention computes CompletedAt and Unseen and maintains seen marks.
func (c *computation) attention(s *Session, b *builder, h *model.HookState, m meta) {
	var comp time.Time
	if h != nil {
		comp = h.TurnEndedAt
	}
	if rc := c.mem.RegCompletions[s.Key]; b.reg != nil && rc.After(comp) {
		comp = rc
	}
	if b.unmanaged && s.Activity == model.Idle {
		switch {
		case m.hasClaude && m.claude.LastStopReason == "end_turn":
			comp = maxTime(comp, m.claude.LastAssistantAt)
		case m.hasAgy:
			comp = maxTime(comp, m.agy.LastStepAt)
		}
	}
	s.CompletedAt = comp
	if !s.Alive {
		return
	}

	seen, ok := c.mem.Seen[s.Key]
	if !ok {
		// First sight. Sessions that started after cccp began watching get
		// their completions flagged from their start; sessions that already
		// existed are treated as acknowledged up to now.
		seen = c.now
		if c.mem.Initialized && s.StartedAt.After(c.watchStart) {
			seen = s.StartedAt
		}
		c.mem.Seen[s.Key] = seen
	}
	if s.Visible && comp.After(seen) {
		seen = c.now
		c.mem.Seen[s.Key] = seen
	}
	s.Unseen = (s.Activity == model.Idle || s.Activity == model.Errored) &&
		comp.After(seen) && !comp.Before(s.LastPromptAt)
}

func groupOf(s Session, now time.Time) Group {
	switch {
	case !s.Alive || s.Activity == model.Ended:
		return GroupClosed
	case s.Activity.NeedsYou():
		return GroupNeedsYou
	case s.Activity == model.Working:
		return GroupWorking
	case s.Activity == model.Idle && s.Unseen:
		return GroupYourTurn
	case s.Activity == model.Idle:
		if s.Cache.Known && !s.Cache.Warm(now) {
			return GroupCold
		}
		return GroupIdle
	}
	return GroupUnknown
}

func sortSessions(ss []Session) {
	sort.SliceStable(ss, func(i, j int) bool {
		a, b := ss[i], ss[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		switch a.Group {
		case GroupNeedsYou, GroupWorking:
			if !a.ActivitySince.Equal(b.ActivitySince) {
				return a.ActivitySince.Before(b.ActivitySince) // waiting longest first
			}
		case GroupYourTurn:
			if !a.CompletedAt.Equal(b.CompletedAt) {
				return a.CompletedAt.Before(b.CompletedAt)
			}
		case GroupIdle:
			// soonest cache expiry first: use it or lose it
			if a.Cache.Known != b.Cache.Known {
				return a.Cache.Known
			}
			if !a.Cache.ExpiresAt.Equal(b.Cache.ExpiresAt) {
				return a.Cache.ExpiresAt.Before(b.Cache.ExpiresAt)
			}
		case GroupCold, GroupUnknown, GroupClosed:
			ta, tb := maxTime(a.ActivitySince, a.LastEventAt), maxTime(b.ActivitySince, b.LastEventAt)
			if !ta.Equal(tb) {
				return ta.After(tb) // most recent first
			}
		}
		return a.Key < b.Key
	})
}
