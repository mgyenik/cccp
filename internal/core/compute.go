package core

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"cccp/internal/config"
	"cccp/internal/jail"
	"cccp/internal/model"
	"cccp/internal/workstream"
)

// Plan returns the transcripts Compute will need for obs. The shell calls it
// on the base observation (hooks, registry, tmux, agents, agy
// conversations), reads what it lists into obs, then calls Compute.
func Plan(obs Observation) Needs { return identify(obs).needs }

// Compute derives the snapshot of all sessions from an observation and
// returns the updated memory. mem is not modified.
//
// watchStart is when the caller began watching: sessions that start later
// are new, and their completions are flagged from their start.
func Compute(obs Observation, mem Memory, cfg config.Config, watchStart, now time.Time) (Snapshot, Memory) {
	c := computation{
		obs:        obs,
		id:         identify(obs),
		cfg:        cfg,
		watchStart: watchStart,
		now:        now,
		mem:        mem.Clone(),
	}
	c.visible = visibleWindows(obs)
	c.noteRegistryTransitions()

	snap := Snapshot{At: now, OnBattery: obs.OnBattery, TmuxOK: obs.Tmux.OK}
	snap.Problems = append(snap.Problems, obs.Problems...)
	if !obs.Tmux.OK && obs.Tmux.Err != "" {
		snap.Problems = append(snap.Problems, "tmux: "+obs.Tmux.Err)
	}
	for _, b := range sortedBuilders(c.id.builders) {
		if s, ok := c.build(b); ok {
			snap.Sessions = append(snap.Sessions, s)
		}
	}
	sortSessions(snap.Sessions)
	if !c.mem.Initialized && len(c.id.builders) > 0 {
		c.mem.Initialized = true
	}
	c.mem.Workstreams = workstream.Observe(c.mem.Workstreams, LiveWorkstreams(obs.Tmux, snap.Sessions), now)
	snap.Workstreams = c.mem.Workstreams.Clone()
	c.mem.Boosts = jail.Expire(c.mem.Boosts, now)
	snap.Jail = jailState(obs, cfg, c.mem.Boosts, now)
	return snap, c.mem
}

// jailState is the jails' state for the running workstreams: their limits
// (a boost, a configured preset, or normal), their numbers, and the moves
// that put the tmux server and each pane where they belong.
func jailState(obs Observation, cfg config.Config, boosts map[string]jail.Boost, now time.Time) Jail {
	switch {
	case !cfg.Jail.Enabled:
		return Jail{Why: "off in the config ([jail] enabled = false)"}
	case !obs.Jail.Available:
		return Jail{Why: obs.Jail.Err}
	}
	s := cfg.Jail.Settings()
	seen := map[string]bool{}
	var names []string
	var panes []jail.Pane
	for _, p := range obs.Tmux.Panes {
		if !seen[p.Session] {
			seen[p.Session] = true
			names = append(names, p.Session)
		}
		panes = append(panes, jail.Pane{ID: p.ID, PID: p.PID, Workstream: p.Session, Cgroup: obs.Jail.PaneCgroups[p.ID]})
	}
	sort.Strings(names)
	desired := s.Desired(names, boosts, now)
	j := Jail{On: true, Root: desired[jail.Root], RootStats: obs.Jail.Stats[jail.Root], Desired: desired, MemTotal: obs.Jail.MemTotal, Presets: s.Presets,
		Moves: jail.Placement(obs.Tmux.ServerPID, obs.Jail.ServerCgroup, panes)}
	for _, n := range names {
		l, preset := s.Effective(n, boosts, now)
		w := JailWorkstream{Name: n, Slice: jail.SliceFor(n), Preset: preset, Limits: l, Stats: obs.Jail.Stats[jail.SliceFor(n)]}
		w.Normal, w.NormalPreset = s.Effective(n, nil, now)
		if b, ok := boosts[n]; ok && b.Active(now) {
			w.Boosted, w.Until = true, b.Until
		}
		j.Workstreams = append(j.Workstreams, w)
	}
	return j
}

// LiveWorkstreams is the running tmux server as the workstream record needs
// it: each pane with the conversation it runs, if any (a live agent session
// with a known id, which can be resumed, or a restore placeholder waiting
// to resume one). sessions may be nil (only placeholders are then known).
func LiveWorkstreams(t TmuxView, sessions []Session) workstream.Live {

	l := workstream.Live{
		OK:      t.OK || t.NoServer,
		Running: t.OK,
		Server:  workstream.Server{PID: t.ServerPID, Start: t.ServerStart, Socket: t.Socket},
	}
	inPane := map[string]*workstream.Conversation{}
	for _, s := range sessions {
		// Only a live session has a pane (classify), and only a known id
		// can be resumed.
		if s.Pane != "" && s.SessionID != "" {
			inPane[s.Pane] = &workstream.Conversation{Agent: s.Agent, SessionID: s.SessionID,
				ConfigDir: s.ConfigDir, DefaultProfile: s.DefaultProfile, Title: s.DisplayTitle()}
		}
	}
	for _, p := range t.Panes {
		lp := workstream.LivePane{Pane: p, Conv: inPane[p.ID]}
		if lp.Conv == nil {
			if c, ok := workstream.ParseTag(p.Resume); ok {
				lp.Conv = c
			}
		}
		l.Panes = append(l.Panes, lp)
	}
	return l
}

// visibleWindows lists windows shown on a tmux client whose terminal has
// focus (model.TmuxClient.Looking: only clients that report focus count).
func visibleWindows(obs Observation) map[string]bool {
	paneWindow := map[string]string{}
	for _, p := range obs.Tmux.Panes {
		paneWindow[p.ID] = p.WindowID
	}
	v := map[string]bool{}
	for _, cl := range obs.Tmux.Clients {
		if w, ok := paneWindow[cl.Pane]; ok && cl.Looking() {
			v[w] = true
		}
	}
	return v
}

// noteRegistryTransitions records a busy/waiting → idle registry transition
// as a completed turn (the completion signal for sessions without hooks),
// and remembers each status for the next comparison.
func (c *computation) noteRegistryTransitions() {
	for _, r := range c.obs.Registry {
		e := r.Entry
		if e.SessionID == "" || !r.Alive || !e.IsUserSession() {
			continue
		}
		key := Key(model.Claude, e.SessionID)
		prev := c.mem.RegLast[key] // zero (no status) on first sight: no transition
		c.mem.RegLast[key] = RegStatus{Status: e.Status, At: e.StatusAt()}
		if (prev.Status == "busy" || prev.Status == "waiting") && e.Status == "idle" {
			if at := e.StatusAt(); at.After(c.mem.RegCompletions[key]) {
				c.mem.RegCompletions[key] = at
			}
		}
	}
}

// NextDeadline is the earliest future time at which the snapshot would
// change with no new observation (a prompt cache expiring, a closed
// workstream due to be forgotten), or zero.
func NextDeadline(s Snapshot) time.Time {
	t := s.Workstreams.Deadline() // a closed workstream to forget
	for _, w := range s.Jail.Workstreams {
		if w.Boosted && w.Until.After(s.At) && (t.IsZero() || w.Until.Before(t)) {
			t = w.Until // a boost to revert
		}
	}
	for _, x := range s.Sessions {
		if x.Group == GroupWorking || x.Group == GroupClosed || !x.Cache.Known {
			continue
		}
		if x.Cache.ExpiresAt.After(s.At) && (t.IsZero() || x.Cache.ExpiresAt.Before(t)) {
			t = x.Cache.ExpiresAt
		}
	}
	return t
}

// NextAttention picks the session to jump to: the next one needing
// attention after the session shown in curPane, in snapshot order (most
// urgent first), wrapping around. It returns false when none needs you.
func NextAttention(s Snapshot, curPane string) (Session, bool) {
	var cands []Session
	for _, x := range s.Sessions {
		if x.NeedsAttention() && x.Alive && (x.Pane != "" || x.Kind == "background") {
			cands = append(cands, x)
		}
	}
	if len(cands) == 0 {
		return Session{}, false
	}
	for i, x := range cands {
		if curPane != "" && x.Pane == curPane {
			return cands[(i+1)%len(cands)], true
		}
	}
	return cands[0], true
}

// Expired lists hook state files that can be deleted: print-mode runs as
// soon as they ended (they are never shown), other sessions once they ended
// longer ago than the retention period.
func Expired(obs Observation, cfg config.Config, now time.Time) []string {
	var out []string
	for _, h := range obs.Hooks {
		st := h.State
		ended := st.Activity == model.Ended || !h.ProcAlive
		if !ended {
			continue
		}
		end := st.EndedAt
		if end.IsZero() {
			end = st.LastEventAt
		}
		if st.PrintMode || now.Sub(end) > cfg.EndedRetention.Duration {
			out = append(out, h.Path)
		}
	}
	return out
}

// Keys returns the set of session keys in a snapshot.
func Keys(s Snapshot) map[string]bool {
	k := make(map[string]bool, len(s.Sessions))
	for _, x := range s.Sessions {
		k[x.Key] = true
	}
	return k
}

// JumpKind is how to show a session.
type JumpKind int

const (
	// JumpPane: switch the client to the session's pane.
	JumpPane JumpKind = iota
	// JumpNewWindow: open a window running Argv in Dir (attach a background
	// session, or resume an ended one).
	JumpNewWindow
	// JumpImpossible: the session cannot be shown; Reason says why.
	JumpImpossible
)

// Jump is the plan for showing a session.
type Jump struct {
	Kind JumpKind
	Pane string
	// For a new window: the directory, command and environment, and the
	// tmux session to open it in ("" = the client's current session).
	Dir         string
	Argv        []string
	Env         []string
	TmuxSession string
	// Resume: the command resumes a closed conversation, which only works
	// in Dir; if Dir no longer exists nothing must be started. (Attaching
	// works from anywhere; Dir is then a convenience.)
	Resume bool
	Reason string
}

// claudeCommand runs claude with args in s's profile. Another profile is
// selected by setting CLAUDE_CONFIG_DIR; the default one by removing it
// (setting it to ~/.claude makes Claude Code use a blank global state, see
// model.Profile.Environ). The removal is part of the command, because a new
// tmux window starts with the tmux server's environment, which may name
// another profile.
func claudeCommand(s Session, args ...string) (argv, env []string) {
	if s.DefaultProfile || s.ConfigDir == "" {
		return append([]string{"env", "-u", model.ConfigDirEnv, "claude"}, args...), nil
	}
	return append([]string{"claude"}, args...), []string{model.ConfigDirEnv + "=" + s.ConfigDir}
}

// PlanJump decides how to show s. panes are the tmux panes, used to open a
// resumed conversation next to the project's other windows.
//
// A Claude session always reopens in its own profile (and account),
// whatever the tmux server's environment (claudeCommand). A resumed conversation must start in its own working
// directory: Claude Code finds the transcript under the project directory
// derived from the cwd it starts in.
func PlanJump(s Session, panes []model.TmuxPane) Jump {
	switch {
	case s.Alive && s.Pane != "":
		return Jump{Kind: JumpPane, Pane: s.Pane}
	case s.Alive && s.Kind == "background" && s.SessionID != "":
		argv, env := claudeCommand(s, "attach", s.SessionID)
		return Jump{Kind: JumpNewWindow, Dir: s.Cwd, Argv: argv, Env: env, TmuxSession: SessionForDir(panes, s.Cwd)}
	case s.Alive:
		// A second process on the same transcript would interleave writes.
		return Jump{Kind: JumpImpossible, Reason: fmt.Sprintf("%s is already open outside tmux (pid %d); close it there first", s.DisplayTitle(), s.PID)}
	case s.SessionID == "":
		return Jump{Kind: JumpImpossible, Reason: s.DisplayTitle() + ": session id unknown, cannot resume"}
	case s.Cwd == "":
		return Jump{Kind: JumpImpossible, Reason: s.DisplayTitle() + ": working directory unknown, cannot resume"}
	case s.Agent == model.Claude:
		argv, env := claudeCommand(s, "--resume", s.SessionID)
		return Jump{Kind: JumpNewWindow, Dir: s.Cwd, Argv: argv, Env: env, TmuxSession: SessionForDir(panes, s.Cwd), Resume: true}
	case s.Agent == model.Agy:
		return Jump{Kind: JumpNewWindow, Dir: s.Cwd, Argv: []string{"agy", "--conversation=" + s.SessionID},
			TmuxSession: SessionForDir(panes, s.Cwd), Resume: true}
	}
	return Jump{Kind: JumpImpossible, Reason: "cannot resume " + string(s.Agent) + " sessions"}
}

// SessionForDir is the tmux session with the most panes in dir (or below
// it), "" when none has any. Ties go to the session name that sorts first.
func SessionForDir(panes []model.TmuxPane, dir string) string {
	if dir == "" {
		return ""
	}
	count := map[string]int{}
	for _, p := range panes {
		if p.Path == dir || strings.HasPrefix(p.Path, strings.TrimSuffix(dir, "/")+"/") {
			count[p.Session]++
		}
	}
	best, n := "", 0
	for sess, c := range count {
		if c > n || (c == n && sess < best) {
			best, n = sess, c
		}
	}
	return best
}

// MoveKind is how to move a session to another workstream.
type MoveKind int

const (
	// MoveWindow: move the session's tmux window (all its panes) as it is.
	MoveWindow MoveKind = iota
	// MoveOpen: open the session in a new window there (Open: attach a
	// background session, or resume a closed conversation).
	MoveOpen
	// MoveImpossible: Reason says why.
	MoveImpossible
)

// Move is the plan for moving a session to the workstream (tmux session)
// dest.
type Move struct {
	Kind   MoveKind
	Window string // MoveWindow: the window id (@N)
	Open   Jump   // MoveOpen: the window to open, its TmuxSession dest
	Reason string
}

// PlanMove decides how to move s into workstream dest. A session in a pane
// takes its whole window along (whatever else is split beside it); one with
// no window of its own is opened there as Enter would open it, but in dest.
func PlanMove(s Session, dest string) Move {
	if s.Alive && s.Pane != "" {
		if s.TmuxSess == dest {
			return Move{Kind: MoveImpossible, Reason: s.DisplayTitle() + " is already in " + dest}
		}
		return Move{Kind: MoveWindow, Window: s.WindowID}
	}
	j := PlanJump(s, nil)
	if j.Kind != JumpNewWindow {
		return Move{Kind: MoveImpossible, Reason: j.Reason}
	}
	j.TmuxSession = dest
	return Move{Kind: MoveOpen, Open: j}
}
