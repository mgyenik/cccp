package engine

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cccp/internal/claudereg"
	"cccp/internal/model"
	"cccp/internal/procs"
	"cccp/internal/tmux"
	"cccp/internal/transcript"
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

var hostname, _ = os.Hostname()

func (e *Engine) build(x *builder, now time.Time, paneByID map[string]tmux.Pane, vis map[string]bool) *Session {
	h, r := x.hook, x.reg
	s := &Session{Key: x.key, Agent: x.agent}
	if !strings.HasPrefix(x.sid, "pane:") {
		s.SessionID = x.sid
	}
	switch {
	case x.unmanaged:
		s.Tracking = TrackUnmanaged
	case h != nil && r != nil:
		s.Tracking = TrackHooksRegistry
	case r != nil:
		s.Tracking = TrackRegistry
	default:
		s.Tracking = TrackHooks
	}
	if x.note != "" {
		s.Notes = append(s.Notes, x.note)
	}

	// Liveness and process.
	switch {
	case r != nil:
		s.Alive, s.PID, s.Kind = true, r.PID, r.Kind
	case x.proc != nil:
		s.Alive, s.PID = true, x.proc.stat.PID
	case h != nil:
		s.PID = h.PID
		if h.PID == 0 {
			// The hook could not identify the agent process; fall back
			// to whether its pane still runs an agent.
			_, ok := paneByID[h.Pane]
			s.Alive = h.Activity != model.Ended && ok && e.paneHasAgent(h.Pane)
		} else {
			s.Alive = h.Activity != model.Ended && !x.superseded && procs.Alive(h.PID, h.PIDStart)
		}
	}
	if s.Kind == "" {
		s.Kind = "interactive"
	}

	// Pane: the process tree is authoritative; then the registry; then the hook.
	pane := ""
	if ap, ok := e.agentsInPanes[s.PID]; ok && s.Alive {
		pane = ap.pane
	} else if r != nil && r.Pane() != "" {
		pane = r.Pane()
	} else if h != nil && s.Alive {
		pane = h.Pane
	}
	var tp tmux.Pane
	if p, ok := paneByID[pane]; ok && s.Alive {
		tp = p
		s.Pane, s.WindowID, s.Location, s.TmuxSess = p.ID, p.WindowID, p.Location(), p.Session
		s.Visible = vis[p.WindowID]
	} else if s.Kind == "background" {
		s.Location = "background"
	}

	// Working directory.
	switch {
	case h != nil && h.Cwd != "":
		s.Cwd = h.Cwd
	case r != nil && r.Cwd != "":
		s.Cwd = r.Cwd
	case s.Alive && s.PID != 0:
		s.Cwd = procs.Cwd(s.PID)
	}

	// Transcript.
	var cm transcript.ClaudeMeta
	var am transcript.AgyMeta
	haveCM, haveAM := false, false
	if s.SessionID != "" {
		hinted := ""
		if h != nil {
			hinted = h.TranscriptPath
		}
		switch x.agent {
		case model.Claude:
			if p := e.claudeTranscript(s.SessionID, s.Cwd, hinted, now); p != "" {
				s.TranscriptPath = p
				if m, err := e.tr.Claude(p); err == nil {
					cm, haveCM = m, true
				}
			}
		case model.Agy:
			p := agyTranscript(s.SessionID, hinted)
			if m, err := e.tr.Agy(p); err == nil {
				am, haveAM = m, true
				s.TranscriptPath = p
			}
		}
	}

	// Activity.
	switch {
	case !s.Alive:
		s.Activity = model.Ended
		if h != nil {
			s.ActivitySince = h.EndedAt
			if s.ActivitySince.IsZero() {
				s.ActivitySince = h.LastEventAt
			}
		}
	case x.unmanaged:
		e.infer(s, x.agent, cm, haveCM, am, haveAM, tp, now)
	default:
		mergeActivity(s, h, r, cm)
	}
	if h != nil {
		s.LastEventAt = h.LastEventAt
		if s.Error == "" {
			s.Error = h.Error
		}
	}
	if r != nil && r.StatusAt().After(s.LastEventAt) {
		s.LastEventAt = r.StatusAt()
	}

	// Times.
	switch {
	case h != nil && !h.StartedAt.IsZero():
		s.StartedAt = h.StartedAt
	case r != nil:
		s.StartedAt = r.StartedAt()
	case x.proc != nil:
		s.StartedAt = procs.StartWall(x.proc.stat.StartTime)
	}
	if h != nil {
		s.EndedAt = h.EndedAt
	}
	if !s.Alive && s.EndedAt.IsZero() {
		s.EndedAt = s.LastEventAt
	}

	// Text: prompt, message, titles.
	e.fillText(s, h, r, cm, haveCM, am, haveAM, tp)

	// Model and context.
	if h != nil {
		s.Model = h.Model
	}
	if haveCM {
		if cm.Model != "" {
			s.Model = cm.Model
		}
		s.ContextTokens = cm.ContextTokens
	} else if haveAM {
		s.ContextTokens = am.ContextTokens
	}

	// Cache (Claude only: agy's caching is not observable).
	if x.agent == model.Claude && haveCM && !cm.LastActivity().IsZero() {
		ttl, assumed := cm.TTL, false
		if ttl == 0 {
			ttl, assumed = e.cfg.DefaultCacheTTL.Duration, true
		}
		last := cm.LastActivity()
		s.Cache = Cache{Known: true, TTL: ttl, TTLAssumed: assumed, LastActivity: last, ExpiresAt: last.Add(ttl)}
	}

	// Completion and seen.
	e.attention(s, x, h, cm, haveCM, am, haveAM, now)

	if rec, ok := e.Persist.Summaries[s.Key]; ok {
		s.Summary = rec.Text
	}
	s.Alias = e.Persist.Aliases[s.Key]

	s.Group = groupOf(s, now)
	if s.Group == GroupEnded && now.Sub(s.EndedAt) > e.cfg.EndedRetention.Duration && !s.EndedAt.IsZero() {
		return nil
	}
	return s
}

func (e *Engine) paneHasAgent(pane string) bool {
	for _, ap := range e.agentsInPanes {
		if ap.pane == pane {
			return true
		}
	}
	return false
}

// mergeActivity applies "latest observation wins" between hooks and the
// registry. When both agree on the coarse class, the hook's finer activity
// and detail are kept.
func mergeActivity(s *Session, h *model.HookState, r *claudereg.Entry, cm transcript.ClaudeMeta) {
	if h == nil && r == nil {
		s.Activity = model.Unknown
		return
	}
	var regAct model.Activity
	var regAt time.Time
	if r != nil {
		regAct, regAt = r.Activity(), r.StatusAt()
	}
	useHook := h != nil && (r == nil || classOf(h.Activity) == classOf(regAct) || !regAt.After(h.ActivityAt))
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
func (e *Engine) infer(s *Session, a model.Agent, cm transcript.ClaudeMeta, haveCM bool, am transcript.AgyMeta, haveAM bool, p tmux.Pane, now time.Time) {
	s.Inferred = true
	quiet := e.cfg.QuietAfter.Duration
	// Output age is only meaningful for single-pane windows: tmux tracks
	// activity per window.
	outAge, outKnown := time.Duration(0), false
	if p.WindowPanes == 1 && !p.WindowActivity.IsZero() {
		outAge, outKnown = now.Sub(p.WindowActivity), true
	}
	switch a {
	case model.Claude:
		switch {
		case haveCM && cm.PendingTool:
			s.Tool = cm.PendingToolName
			if outKnown && outAge >= quiet {
				// A running tool keeps Claude's spinner (and so the pane)
				// updating; a pending tool with a silent pane is waiting
				// for permission.
				s.Activity, s.ActivitySince = model.NeedsApproval, maxTime(cm.LastAssistantAt, p.WindowActivity)
				s.Question = "tool call pending and the pane is quiet: probably waiting for approval"
			} else {
				s.Activity, s.ActivitySince = model.Working, cm.LastAssistantAt
			}
		case haveCM && cm.LastUserAt.After(cm.LastAssistantAt) && !cm.Interrupted:
			if outKnown && outAge >= time.Minute {
				s.Activity, s.ActivitySince = model.Unknown, cm.LastUserAt
				s.Notes = append(s.Notes, "prompt sent, no response yet, pane quiet")
			} else {
				s.Activity, s.ActivitySince = model.Working, cm.LastUserAt
			}
		case haveCM && !cm.LastAssistantAt.IsZero():
			s.Activity, s.ActivitySince = model.Idle, maxTime(cm.LastAssistantAt, cm.LastUserAt)
		case outKnown && outAge < quiet:
			s.Activity, s.ActivitySince = model.Working, p.WindowActivity
			s.Notes = append(s.Notes, "no transcript; pane is producing output")
		default:
			s.Activity, s.ActivitySince = model.Unknown, p.WindowActivity
			s.Notes = append(s.Notes, "no transcript (Claude Code deletes transcripts after cleanupPeriodDays)")
		}
	case model.Agy:
		// agy redraws its pane continuously even when idle, so pane output
		// says nothing; only the transcript is used.
		switch {
		case haveAM && am.LastStepType == "PLANNER_RESPONSE" && !am.PendingTools && am.LastStepStatus == "DONE":
			s.Activity, s.ActivitySince = model.Idle, am.LastStepAt
		case haveAM && am.LastStepStatus != "" && am.LastStepStatus != "DONE":
			s.Activity, s.ActivitySince = model.Working, am.LastStepAt
		default:
			s.Activity = model.Unknown
			if haveAM {
				s.ActivitySince = am.LastStepAt
			}
		}
	}
}

func (e *Engine) fillText(s *Session, h *model.HookState, r *claudereg.Entry, cm transcript.ClaudeMeta, haveCM bool, am transcript.AgyMeta, haveAM bool, p tmux.Pane) {
	// Last prompt / message: whichever source is newest.
	if h != nil {
		s.LastPrompt, s.LastPromptAt = h.LastPrompt, h.LastPromptAt
		s.LastMessage = h.LastMessage
	}
	msgAt := time.Time{}
	if h != nil {
		msgAt = h.LastMessageAt
	}
	if haveCM {
		if cm.LastPrompt != "" && (s.LastPrompt == "" || cm.LastPromptAt.After(s.LastPromptAt)) {
			s.LastPrompt = cm.LastPrompt
			if !cm.LastPromptAt.IsZero() {
				s.LastPromptAt = cm.LastPromptAt
			}
		}
		if cm.LastAssistantText != "" && (s.LastMessage == "" || cm.LastAssistantAt.After(msgAt.Add(2*time.Second))) {
			s.LastMessage = cm.LastAssistantText
		}
	}
	if haveAM {
		if am.LastPrompt != "" && (s.LastPrompt == "" || am.LastPromptAt.After(s.LastPromptAt)) {
			s.LastPrompt, s.LastPromptAt = am.LastPrompt, am.LastPromptAt
		}
		if am.LastMessage != "" {
			s.LastMessage = am.LastMessage
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
	if haveCM {
		cs = append(cs, cand{cm.AITitle, "ai-title"})
	}
	if s.Agent == model.Agy && s.SessionID != "" {
		if c, ok := e.agydb.All()[s.SessionID]; ok {
			cs = append(cs, cand{c.Title, "agy"}, cand{firstLine(c.Preview), "agy-preview"})
		}
		if haveAM {
			cs = append(cs, cand{firstLine(am.LastPrompt), "prompt"})
		}
	}
	if pt := cleanPaneTitle(p.Title); pt != "" && pt != hostname && !strings.HasPrefix(pt, hostname+".") {
		cs = append(cs, cand{pt, "pane"})
	}
	if r != nil {
		cs = append(cs, cand{r.Name, "name"})
	}
	if s.Cwd != "" {
		cs = append(cs, cand{filepath.Base(s.Cwd), "cwd"})
	}
	for _, c := range cs {
		if strings.TrimSpace(c.v) != "" {
			s.Title, s.TitleSource = strings.TrimSpace(c.v), c.src
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

// attention computes CompletedAt / Unseen and maintains seen marks.
func (e *Engine) attention(s *Session, x *builder, h *model.HookState, cm transcript.ClaudeMeta, haveCM bool, am transcript.AgyMeta, haveAM bool, now time.Time) {
	var comp time.Time
	if h != nil {
		comp = h.TurnEndedAt
	}
	if rc := e.Persist.RegCompletions[s.Key]; rc.After(comp) && x.reg != nil {
		comp = rc
	}
	if x.unmanaged && s.Activity == model.Idle {
		switch {
		case haveCM && cm.LastStopReason == "end_turn":
			comp = maxTime(comp, cm.LastAssistantAt)
		case haveAM:
			comp = maxTime(comp, am.LastStepAt)
		}
	}
	s.CompletedAt = comp
	if !s.Alive {
		return
	}

	seen, ok := e.Persist.Seen[s.Key]
	if !ok {
		// First sight. Sessions that started after this engine began
		// watching get their completions flagged from the start;
		// pre-existing ones are treated as acknowledged up to now.
		seen = now
		if e.Persist.Initialized && s.StartedAt.After(e.daemonStart) {
			seen = s.StartedAt
		}
		e.Persist.Seen[s.Key] = seen
		e.PersistDirty = true
	}
	if s.Visible && comp.After(seen) {
		seen = now
		e.Persist.Seen[s.Key] = seen
		e.PersistDirty = true
	}
	s.Unseen = (s.Activity == model.Idle || s.Activity == model.Errored) &&
		comp.After(seen) && !comp.Before(s.LastPromptAt)
}

func groupOf(s *Session, now time.Time) Group {
	switch {
	case !s.Alive || s.Activity == model.Ended:
		return GroupEnded
	case s.Activity.NeedsYou():
		return GroupNeedsYou
	case s.Activity == model.Working:
		return GroupWorking
	case s.Activity == model.Idle && s.Unseen:
		return GroupYourTurn
	case s.Activity == model.Idle:
		if s.Agent == model.Claude && s.Cache.Known && !s.Cache.Warm(now) {
			return GroupCold
		}
		return GroupIdle
	}
	return GroupUnknown
}

func sortSessions(ss []Session, now time.Time) {
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
		case GroupCold, GroupUnknown, GroupEnded:
			ta, tb := maxTime(a.ActivitySince, a.LastEventAt), maxTime(b.ActivitySince, b.LastEventAt)
			if !ta.Equal(tb) {
				return ta.After(tb) // most recent first
			}
		}
		return a.Key < b.Key
	})
}
