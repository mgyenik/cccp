package core

import (
	"time"

	"cccp/internal/config"
	"cccp/internal/model"
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
	return snap, c.mem
}

// visibleWindows lists windows shown on a focused tmux client.
func visibleWindows(obs Observation) map[string]bool {
	paneWindow := map[string]string{}
	for _, p := range obs.Tmux.Panes {
		paneWindow[p.ID] = p.WindowID
	}
	v := map[string]bool{}
	for _, cl := range obs.Tmux.Clients {
		if w, ok := paneWindow[cl.Pane]; ok && cl.Focused {
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
// change with no new observation (a prompt cache expiring), or zero.
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
	Kind   JumpKind
	Pane   string
	Dir    string
	Argv   []string
	Reason string
}

// PlanJump decides how to show s.
func PlanJump(s Session) Jump {
	switch {
	case s.Alive && s.Pane != "":
		return Jump{Kind: JumpPane, Pane: s.Pane}
	case s.Alive && s.Kind == "background" && s.SessionID != "":
		return Jump{Kind: JumpNewWindow, Dir: s.Cwd, Argv: []string{"claude", "attach", s.SessionID}}
	case s.Alive:
		return Jump{Kind: JumpImpossible, Reason: s.DisplayTitle() + " has no tmux pane"}
	case s.SessionID == "":
		return Jump{Kind: JumpImpossible, Reason: s.DisplayTitle() + ": session id unknown, cannot resume"}
	case s.Agent == model.Claude:
		return Jump{Kind: JumpNewWindow, Dir: s.Cwd, Argv: []string{"claude", "--resume", s.SessionID}}
	case s.Agent == model.Agy:
		return Jump{Kind: JumpNewWindow, Dir: s.Cwd, Argv: []string{"agy", "--conversation=" + s.SessionID}}
	}
	return Jump{Kind: JumpImpossible, Reason: "cannot resume " + string(s.Agent) + " sessions"}
}
