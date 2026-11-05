// Package machine is the daemon's logic as a deterministic state machine.
//
// The daemon shell feeds it Inputs (an observation of the world, a timer
// firing, a finished summary, a user request) and executes the Effects it
// returns (publish a snapshot, set a tmux option, notify, start a summary,
// save memory, delete files, arm the timer). The machine performs no I/O,
// starts no goroutines and never reads the clock: every time arrives in an
// Input. Each behaviour of the daemon is therefore tested by feeding inputs
// and checking effects.
package machine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"cccp/internal/config"
	"cccp/internal/core"
	"cccp/internal/model"
	"cccp/internal/present"
)

// Policy constants.
const (
	// SaveDelay batches memory writes.
	SaveDelay = 5 * time.Second
	// PruneEvery is how often expired state files and memory are pruned.
	PruneEvery = 6 * time.Hour
)

// ---- inputs -----------------------------------------------------------------

// Input is something that happened.
type Input interface{ isInput() }

// Observed: a new observation of the world.
type Observed struct {
	Obs core.Observation
	At  time.Time
}

// TimerFired: the timer armed by ArmTimer fired.
type TimerFired struct{ At time.Time }

// SummaryDone: a StartSummary finished.
type SummaryDone struct {
	Key, Hash string
	Text      string
	Err       error
	At        time.Time
}

// MarkSeen: the user acknowledged a session.
type MarkSeen struct {
	Key string
	At  time.Time
}

// SetAlias: the user named a session ("" clears).
type SetAlias struct {
	Key, Value string
	At         time.Time
}

// ConfigChanged: the configuration was reloaded.
type ConfigChanged struct {
	Config config.Config
	At     time.Time
}

// TmuxFailed: setting a tmux option failed; everything is re-sent with the
// next snapshot.
type TmuxFailed struct{}

// RequestSummary: the user selected a closed conversation without a summary.
type RequestSummary struct {
	Key string
	At  time.Time
}

// Stop: the daemon is shutting down.
type Stop struct{ At time.Time }

func (Observed) isInput()       {}
func (TimerFired) isInput()     {}
func (SummaryDone) isInput()    {}
func (MarkSeen) isInput()       {}
func (SetAlias) isInput()       {}
func (ConfigChanged) isInput()  {}
func (TmuxFailed) isInput()     {}
func (RequestSummary) isInput() {}
func (Stop) isInput()           {}

// ---- effects ----------------------------------------------------------------

// Effect is something the shell must do.
type Effect interface{ isEffect() }

// Publish: send the snapshot to subscribers and pending requests.
type Publish struct{ Snapshot core.Snapshot }

// SetStatus: set the tmux status-line segment ("" unsets it).
type SetStatus struct{ Value string }

// SetGlyph: set a window's state glyph ("" unsets it).
type SetGlyph struct{ Window, Value string }

// Notify: run the notification command.
type Notify struct {
	Key  string
	Argv []string
}

// StartSummary: generate a summary; report back with SummaryDone.
// Profile is the Claude profile to run it under: the session's own, so
// the summary is billed to, and the session's text only ever sent through,
// that session's account.
type StartSummary struct {
	Key, Hash, Input, Model string
	Profile                 model.Profile
	SystemPrompt            string
}

// SaveMemory: persist memory.
type SaveMemory struct{ Memory core.Memory }

// RemoveFiles: delete expired hook state files.
type RemoveFiles struct{ Paths []string }

// ArmTimer: deliver TimerFired at At; a zero At disarms.
type ArmTimer struct{ At time.Time }

// Log: write a log line.
type Log struct{ Msg string }

func (Publish) isEffect()      {}
func (SetStatus) isEffect()    {}
func (SetGlyph) isEffect()     {}
func (Notify) isEffect()       {}
func (StartSummary) isEffect() {}
func (SaveMemory) isEffect()   {}
func (RemoveFiles) isEffect()  {}
func (ArmTimer) isEffect()     {}
func (Log) isEffect()          {}

// ---- state ------------------------------------------------------------------

// Machine is the daemon's state.
type Machine struct {
	cfg        config.Config
	mem        core.Memory
	watchStart time.Time

	obs     core.Observation
	haveObs bool
	snap    core.Snapshot
	have    bool

	// tmux decoration as last applied.
	tmuxOK    bool
	statusSet bool
	status    string
	windows   map[string]string

	// notified holds, per session, the attention state last notified (or
	// present when watching began, so existing states never notify).
	notified map[string]string
	// held: automatic turns' notifications waiting for their summary.
	held map[string]held

	sum *sumQueue
	// requested: closed conversations the user asked to summarise.
	requested map[string]bool

	js jailState

	dirty     bool
	saveDue   time.Time
	lastPrune time.Time
	timerAt   time.Time
	stopped   bool
}

// New creates a machine. start is when watching began.
func New(cfg config.Config, mem core.Memory, start time.Time) *Machine {
	return &Machine{
		cfg:        cfg,
		mem:        mem.Clone(),
		watchStart: start,
		windows:    map[string]string{},
		held:       map[string]held{},
		sum:        newSumQueue(),
		requested:  map[string]bool{},
		js:         newJailState(),
	}
}

// Snapshot is the last published snapshot.
func (m *Machine) Snapshot() (core.Snapshot, bool) { return m.snap, m.have }

// Memory is the current memory (a copy).
func (m *Machine) Memory() core.Memory { return m.mem.Clone() }

// Config is the current configuration.
func (m *Machine) Config() config.Config { return m.cfg }

// Step applies one input and returns the effects to execute, in order.
func (m *Machine) Step(in Input) []Effect {
	if m.stopped {
		return nil
	}
	switch in := in.(type) {
	case Observed:
		m.obs, m.haveObs = in.Obs, true
		return m.recompute(in.At)
	case TimerFired:
		return m.recompute(in.At)
	case SummaryDone:
		return m.summaryDone(in)
	case MarkSeen:
		mem := m.mem.Clone()
		mem.Seen[in.Key] = in.At
		m.setMem(mem, in.At)
		return m.recompute(in.At)
	case SetAlias:
		mem := m.mem.Clone()
		if v := strings.TrimSpace(in.Value); v == "" {
			delete(mem.Aliases, in.Key)
		} else {
			mem.Aliases[in.Key] = v
		}
		m.setMem(mem, in.At)
		return m.recompute(in.At)
	case ConfigChanged:
		m.cfg = in.Config
		return m.recompute(in.At)
	case TmuxFailed:
		m.tmuxOK = false
		return nil
	case RequestSummary:
		m.requested[in.Key] = true
		return m.recompute(in.At)
	case SetBoost:
		m.setBoost(in)
		return m.recompute(in.At)
	case JailDone:
		return append(m.jailDone(in), m.arm(m.snap)...)
	case JailStatsRead:
		return m.jailStatsRead(in)
	case Stop:
		return m.stop()
	}
	return []Effect{Log{Msg: fmt.Sprintf("machine: unknown input %T", in)}}
}

// setMem replaces memory, scheduling a save if the persisted part changed.
func (m *Machine) setMem(mem core.Memory, now time.Time) {
	if !mem.Persisted(m.mem) && !m.dirty {
		m.dirty = true
		m.saveDue = now.Add(SaveDelay)
	}
	m.mem = mem
}

// recompute derives a new snapshot and every effect that follows from it.
func (m *Machine) recompute(now time.Time) []Effect {
	if !m.haveObs {
		return nil
	}
	snap, mem := core.Compute(m.obs, m.mem, m.cfg, m.watchStart, now)
	snap.FromDaemon = true
	m.setMem(mem, now)
	hadPrev := m.have
	wasAlive := map[string]bool{}
	for _, s := range m.snap.Sessions {
		if s.Alive {
			wasAlive[s.Key] = true
		}
	}
	m.snap, m.have = snap, true

	var effs []Effect
	effs = append(effs, m.decorate(snap)...)
	if hadPrev {
		effs = append(effs, m.notify(snap)...)
	} else {
		m.prime(snap)
	}
	effs = append(effs, m.summaries(snap, wasAlive, now)...)
	effs = append(effs, m.jail(snap, now)...)
	effs = append(effs, Publish{Snapshot: snap})
	effs = append(effs, m.prune(snap, now)...)
	if m.dirty && !now.Before(m.saveDue) {
		effs = append(effs, SaveMemory{Memory: m.mem.Clone()})
		m.dirty = false
	}
	return append(effs, m.arm(snap)...)
}

// ---- tmux decoration ----------------------------------------------------------

func (m *Machine) decorate(snap core.Snapshot) []Effect {
	if !snap.TmuxOK {
		m.tmuxOK = false
		return nil
	}
	if !m.tmuxOK {
		// A (re)started server, or a failed write: assume nothing is set.
		m.statusSet, m.status = false, ""
		m.windows = map[string]string{}
		m.tmuxOK = true
	}
	var effs []Effect
	if st := present.StatusSegment(snap); !m.statusSet || st != m.status {
		effs = append(effs, SetStatus{Value: st})
		m.status, m.statusSet = st, true
	}
	want := present.WindowGlyphs(snap)
	for _, w := range sortedKeys(want) {
		if g := want[w]; m.windows[w] != g {
			effs = append(effs, SetGlyph{Window: w, Value: g})
			m.windows[w] = g
		}
	}
	for _, w := range sortedKeys(m.windows) {
		if _, ok := want[w]; !ok {
			effs = append(effs, SetGlyph{Window: w, Value: ""})
			delete(m.windows, w)
		}
	}
	return effs
}

// ---- notifications --------------------------------------------------------------

// signature identifies an attention state for de-duplication; "" when the
// session needs no attention.
func signature(s core.Session) string {
	if !s.NeedsAttention() || !s.Alive {
		return ""
	}
	return fmt.Sprintf("%d|%s|%d", s.Group, s.Activity, s.ActivitySince.UnixMilli())
}

// prime records the states present when watching began, so they never
// notify.
func (m *Machine) prime(snap core.Snapshot) {
	m.notified = map[string]string{}
	for _, s := range snap.Sessions {
		if sig := signature(s); sig != "" {
			m.notified[s.Key] = sig
		}
	}
}

// HoldForSummary bounds how long an automatic turn's notification waits
// for the summary that decides it.
const HoldForSummary = 2 * time.Minute

// held is an automatic turn's notification waiting for its summary.
type held struct {
	sig   string
	since time.Time
}

// notify emits a notification for each session that newly needs attention
// while its pane is not visible.
//
// A turn the user started notifies at once. A turn that may be waiting on
// background work (Session.Background: Claude Code started it on a
// background report, or a background shell still runs) is usually a
// progress report: its notification waits for the turn's summary and goes
// out only if the summary says there is something to do (VerdictAct). It goes out anyway
// if no summary can come (summaries off or not allowed, failing or paused)
// or none has come within HoldForSummary.
func (m *Machine) notify(snap core.Snapshot) []Effect {
	var effs []Effect
	live := map[string]bool{}
	for _, s := range snap.Sessions {
		live[s.Key] = true
		sig := signature(s)
		if sig == "" {
			delete(m.notified, s.Key)
			delete(m.held, s.Key)
			continue
		}
		if m.notified[s.Key] == sig {
			continue
		}
		title, body := notificationText(s, len(m.obs.Profiles) > 1)
		what := fmt.Sprintf("%s (%s, %s): %s — %s", s.Key, s.Group, orDash(s.Location), title, body)
		if !m.cfg.Notify || len(m.cfg.NotifyCommand) == 0 || s.Visible {
			m.notified[s.Key] = sig
			delete(m.held, s.Key)
			if s.Visible {
				// Every attention state leaves a trace in the log, so a
				// missing notification can be told from a missed one.
				effs = append(effs, Log{Msg: "not notifying (pane visible) " + what})
			}
			continue
		}
		if s.Group == core.GroupYourTurn && s.Background() {
			if s.Summary == "" && m.summaryComing(s, snap.OnBattery, snap.At) {
				h, ok := m.held[s.Key]
				if !ok || h.sig != sig {
					h = held{sig: sig, since: snap.At}
					m.held[s.Key] = h
				}
				if snap.At.Sub(h.since) < HoldForSummary {
					continue
				}
			}
			delete(m.held, s.Key)
			if s.SummaryVerdict == core.VerdictWait {
				m.notified[s.Key] = sig
				effs = append(effs, Log{Msg: "not notifying (background work, nothing to do) " + what})
				continue
			}
		}
		m.notified[s.Key] = sig
		argv := append(append([]string{}, m.cfg.NotifyCommand...), title, body)
		effs = append(effs, Log{Msg: "notify " + what}, Notify{Key: s.Key, Argv: argv})
	}
	for k := range m.notified {
		if !live[k] {
			delete(m.notified, k)
		}
	}
	for k := range m.held {
		if !live[k] {
			delete(m.held, k)
		}
	}
	return effs
}

// summaryComing reports whether a summary of s as it is now is on its way:
// one will be offered (summaries() would) and has neither failed nor been
// paused.
func (m *Machine) summaryComing(s core.Session, onBattery bool, now time.Time) bool {
	if !s.Alive || (s.LastMessage == "" && s.Question == "") {
		return false
	}
	prof, ok := m.summaryProfile(s)
	if !ok || !m.summariesAllowed(prof.Summaries, onBattery) {
		return false
	}
	_, hash := core.SummaryInput(s)
	return !m.sum.stalled(s.Key, hash, now)
}

// heldDeadline is when the first held notification stops waiting.
func (m *Machine) heldDeadline() time.Time {
	var t time.Time
	for _, h := range m.held {
		if d := h.since.Add(HoldForSummary); t.IsZero() || d.Before(t) {
			t = d
		}
	}
	return t
}

func notificationText(s core.Session, showProfile bool) (title, body string) {
	title = "cccp: " + s.Group.String() + " — " + present.TitleWithProfile(s, showProfile)
	body = present.Detail(s)
	if s.Inferred {
		body = "(inferred) " + body
	}
	if s.Location != "" {
		body = s.Location + "  " + body
	}
	return title, body
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- summaries --------------------------------------------------------------------

// Summary system prompts: for a session that needs you, and for a closed
// conversation.
const (
	LiveSummaryPrompt = "You write one-line status lines for a dashboard of coding-agent sessions. " +
		"Given a session's title and the assistant's last message (and, for a turn the user started, the " +
		"user's prompt), decide whether the user has something to do now, then say in one line under 90 " +
		"characters what the session needs from the user next, then end the line with \" [act]\" or \" [wait]\".\n" +
		"[act]: the assistant's last message asks the user a question, asks for a decision or approval, tells " +
		"the user to do something (run a command, review a result), or says new work is ready for the user to try.\n" +
		"[wait]: it only reports progress, says agents or tasks are still running, or says what the assistant " +
		"itself will do next.\n" +
		"No preamble, no quotes, no trailing period."
	// The verdict comes last, after the line is written: asked for first, it
	// was measured to contradict the line it then wrote (docs/notifications.md).
	ClosedSummaryPrompt = "You write one-line descriptions for a list of past coding-agent conversations. " +
		"Given a conversation's title, the user's first and last prompts and the assistant's last message, " +
		"say in one line under 90 characters what the conversation was about and where it left off. " +
		"No preamble, no quotes, no trailing period."
)

// summaryProfile is the profile a session's summary runs under: its own,
// or (for agy sessions, which have none) the default profile.
func (m *Machine) summaryProfile(s core.Session) (model.Profile, bool) {
	for _, p := range m.obs.Profiles {
		if (s.ConfigDir != "" && p.Dir == s.ConfigDir) || (s.ConfigDir == "" && p.Default) {
			return p, true
		}
	}
	return model.Profile{}, false
}

// summariesAllowed applies a policy ("" = the global one) to the power state.
func (m *Machine) summariesAllowed(policy string, onBattery bool) bool {
	if policy == "" {
		policy = m.cfg.Summaries
	}
	switch policy {
	case "always":
		return true
	case "ac":
		return !onBattery
	}
	return false
}

// summaries offers summary jobs: for sessions that need you, and for closed
// conversations that just closed (wasAlive) or that the user asked about.
func (m *Machine) summaries(snap core.Snapshot, wasAlive map[string]bool, now time.Time) []Effect {
	for _, s := range snap.Sessions {
		var in, hash, prompt string
		switch {
		case s.Alive && s.NeedsAttention() && (s.LastMessage != "" || s.Question != ""):
			in, hash = core.SummaryInput(s)
			prompt = LiveSummaryPrompt
		case s.Group == core.GroupClosed && (wasAlive[s.Key] || m.requested[s.Key]):
			delete(m.requested, s.Key)
			if s.FirstPrompt == "" && s.LastMessage == "" {
				continue // nothing to summarise
			}
			in, hash = core.SummaryInput(s)
			prompt = ClosedSummaryPrompt
		default:
			continue
		}
		prof, ok := m.summaryProfile(s)
		if !ok || !m.summariesAllowed(prof.Summaries, snap.OnBattery) {
			continue // no known profile to run under, or not allowed
		}
		if rec, ok := m.mem.Summaries[s.Key]; ok && rec.InputHash == hash {
			continue
		}
		m.sum.offer(summaryJob{key: s.Key, hash: hash, input: in, profile: prof, prompt: prompt}, now)
	}
	for k := range m.requested {
		delete(m.requested, k) // requests for keys not in the snapshot
	}
	m.sum.forget(core.Keys(snap))
	return m.startSummary(now)
}

func (m *Machine) startSummary(now time.Time) []Effect {
	job, ok := m.sum.next(now)
	if !ok {
		return nil
	}
	return []Effect{StartSummary{Key: job.key, Hash: job.hash, Input: job.input, Model: m.cfg.SummaryModel,
		Profile: job.profile, SystemPrompt: job.prompt}}
}

func (m *Machine) summaryDone(in SummaryDone) []Effect {
	var effs []Effect
	if m.sum.done(in.Key, in.Hash, in.Err != nil, in.At) {
		effs = append(effs, Log{Msg: fmt.Sprintf("summaries paused for %v after %d consecutive failures", pauseFor, pauseAfter)})
	}
	if in.Err != nil {
		effs = append(effs, Log{Msg: fmt.Sprintf("summary for %s failed: %v", in.Key, in.Err)})
	} else if verdict, text := splitVerdict(firstLine(in.Text)); cleanSummary(text) != "" {
		text = cleanSummary(text)
		mem := m.mem.Clone()
		mem.Summaries[in.Key] = core.SummaryRecord{InputHash: in.Hash, Text: text, At: in.At, Verdict: verdict}
		m.setMem(mem, in.At)
	}
	return append(effs, m.recompute(in.At)...)
}

// splitVerdict takes a live summary's closing "[act]" or "[wait]" off its
// line (LiveSummaryPrompt); without one the verdict is "".
func splitVerdict(s string) (verdict, text string) {
	t := strings.TrimSpace(s)
	for _, v := range []string{core.VerdictAct, core.VerdictWait} {
		tag := "[" + v + "]"
		// t is trimmed and longer than the tag: text before it is never empty.
		if len(t) > len(tag) && strings.EqualFold(t[len(t)-len(tag):], tag) {
			return v, strings.TrimSpace(t[:len(t)-len(tag)])
		}
	}
	return "", s
}

// firstLine is s's first non-empty line, trimmed.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// cleanSummary keeps the first line, unquoted, at most 120 characters.
func cleanSummary(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.Trim(s, `"`)
	if r := []rune(s); len(r) > 120 {
		s = string(r[:119]) + "…"
	}
	return s
}

// ---- pruning, timer, shutdown ----------------------------------------------------

func (m *Machine) prune(snap core.Snapshot, now time.Time) []Effect {
	if !m.lastPrune.IsZero() && now.Sub(m.lastPrune) < PruneEvery {
		return nil
	}
	m.lastPrune = now
	var effs []Effect
	if paths := core.Expired(m.obs, m.cfg, now); len(paths) > 0 {
		effs = append(effs, RemoveFiles{Paths: paths}, Log{Msg: fmt.Sprintf("pruning %d expired state files", len(paths))})
	}
	m.setMem(m.mem.Prune(core.Keys(snap)), now)
	return effs
}

// arm sets the timer to the earliest deadline: a cache expiring or a
// pending save. It emits ArmTimer only when the deadline changes.
func (m *Machine) arm(snap core.Snapshot) []Effect {
	at := core.NextDeadline(snap)
	if m.dirty && (at.IsZero() || m.saveDue.Before(at)) {
		at = m.saveDue
	}
	if r := m.jailDeadline(snap.At); !r.IsZero() && (at.IsZero() || r.Before(at)) {
		at = r
	}
	if h := m.heldDeadline(); !h.IsZero() && (at.IsZero() || h.Before(at)) {
		at = h
	}
	if at.Equal(m.timerAt) {
		return nil
	}
	m.timerAt = at
	return []Effect{ArmTimer{At: at}}
}

func (m *Machine) stop() []Effect {
	m.stopped = true
	effs := []Effect{SaveMemory{Memory: m.mem.Clone()}, ArmTimer{}}
	if m.tmuxOK {
		if m.statusSet && m.status != "" {
			effs = append(effs, SetStatus{Value: ""})
		}
		for _, w := range sortedKeys(m.windows) {
			effs = append(effs, SetGlyph{Window: w, Value: ""})
		}
	}
	return effs
}

func sortedKeys[V any](mm map[string]V) []string {
	k := make([]string, 0, len(mm))
	for x := range mm {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
