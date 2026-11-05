package machine

import (
	"fmt"
	"sort"
	"time"

	"cccp/internal/core"
	"cccp/internal/jail"
	"cccp/internal/present"
)

// JailRetry is how long a failed slice configuration or move waits before
// it is tried again (not on every observation: tmux events come in bursts).
const JailRetry = time.Minute

// SetBoost gives a workstream a preset for a while (For 0: until undone),
// or with Clear takes its boost away.
type SetBoost struct {
	Workstream string
	Preset     string
	For        time.Duration
	Clear      bool
	At         time.Time
}

// JailDone reports a JailLimits or JailMove effect that failed (Err), so it
// is retried later instead of taken as done.
type JailDone struct {
	Unit string // the slice, for JailLimits
	Pane string // the pane ("" the tmux server), for JailMove
	From string
	Err  error
	At   time.Time
}

// JailLimits: give a slice these limits (creating it if need be).
type JailLimits struct {
	Unit   string
	Limits jail.Limits
}

// JailMove: move processes into a new scope in a slice (jail.Move).
type JailMove struct{ Move jail.Move }

// JailStatsRead delivers the slices' numbers between observations (memory
// grows with no event to observe), for warnings near a limit and kills at
// it.
type JailStatsRead struct {
	Stats map[string]jail.Stats // by slice
	At    time.Time
}

func (JailStatsRead) isInput() {}

func (SetBoost) isInput()    {}
func (JailDone) isInput()    {}
func (JailLimits) isEffect() {}
func (JailMove) isEffect()   {}

// jailState is what the machine has done to the jails.
type jailState struct {
	applied   map[string]jail.Limits // by unit, as last set
	retryAt   map[string]time.Time   // a failed unit or move ("pane|from"), not before
	triedMove map[string]time.Time   // moves made, by "pane|from"
	oomKills  map[string]int64       // by slice, last seen (a slice first seen notifies nothing)
	warned    map[string]bool        // by slice: warned near its limit, not yet back below Rearm
}

func newJailState() jailState {
	return jailState{applied: map[string]jail.Limits{}, retryAt: map[string]time.Time{},
		triedMove: map[string]time.Time{}, oomKills: map[string]int64{}, warned: map[string]bool{}}
}

func moveKey(pane, from string) string { return pane + "|" + from }

// jail returns the effects that bring the slices and placement in line with
// the snapshot: limits that differ from what was last applied (A first, so
// a workstream's slice is never made without A above it), each move not
// tried within JailRetry, and a notification per kill at a workstream's
// hard limit.
func (m *Machine) jail(snap core.Snapshot, now time.Time) []Effect {
	j := snap.Jail
	if !j.On {
		m.js = newJailState()
		return nil
	}
	var effs []Effect
	units := make([]string, 0, len(j.Desired))
	for u := range j.Desired {
		units = append(units, u)
	}
	sort.Slice(units, func(a, b int) bool {
		if (units[a] == jail.Root) != (units[b] == jail.Root) {
			return units[a] == jail.Root
		}
		return units[a] < units[b]
	})
	for _, u := range units {
		l := j.Desired[u]
		if cur, ok := m.js.applied[u]; ok && cur == l {
			continue
		}
		if now.Before(m.js.retryAt[u]) {
			continue
		}
		m.js.applied[u] = l
		effs = append(effs, JailLimits{Unit: u, Limits: l})
	}
	// Every move sets its slice's limits again first: a slice that went
	// away (stopped by someone) would otherwise be made again by the move,
	// implicitly and without limits.
	set := map[string]bool{}
	for _, e := range effs {
		set[e.(JailLimits).Unit] = true
	}
	live := map[string]bool{}
	for _, mv := range j.Moves {
		k := moveKey(mv.Pane, mv.From)
		live[k] = true
		if t, ok := m.js.triedMove[k]; ok && now.Sub(t) < JailRetry {
			continue // made, and the next observation shows the result
		}
		if now.Before(m.js.retryAt[k]) {
			continue
		}
		m.js.triedMove[k] = now
		for _, u := range []string{jail.Root, mv.Slice} {
			if l, ok := j.Desired[u]; ok && !set[u] {
				set[u] = true
				m.js.applied[u] = l
				effs = append(effs, JailLimits{Unit: u, Limits: l})
			}
		}
		effs = append(effs, JailMove{Move: mv})
	}
	for k := range m.js.triedMove {
		if !live[k] {
			delete(m.js.triedMove, k) // done
		}
	}
	effs = append(effs, m.jailWatch(j.Workstreams)...)
	return effs
}

// jailDone takes a failed effect back, to be tried again after JailRetry.
func (m *Machine) jailDone(in JailDone) []Effect {
	if in.Err == nil {
		return nil
	}
	k := in.Unit
	if k == "" {
		k = moveKey(in.Pane, in.From)
		delete(m.js.triedMove, k)
	} else {
		delete(m.js.applied, k)
	}
	m.js.retryAt[k] = in.At.Add(JailRetry)
	return []Effect{Log{Msg: fmt.Sprintf("jail: %v (retrying in %v)", in.Err, JailRetry)}}
}

// jailDeadline is the earliest retry due.
func (m *Machine) jailDeadline(now time.Time) time.Time {
	var t time.Time
	for _, r := range m.js.retryAt {
		if r.After(now) && (t.IsZero() || r.Before(t)) {
			t = r
		}
	}
	return t
}

// setBoost changes a workstream's boost in memory.
func (m *Machine) setBoost(in SetBoost) {
	mem := m.mem.Clone()
	if in.Clear {
		delete(mem.Boosts, in.Workstream)
	} else {
		b := jail.Boost{Preset: in.Preset}
		if in.For > 0 {
			b.Until = in.At.Add(in.For)
		}
		mem.Boosts[in.Workstream] = b
	}
	m.setMem(mem, in.At)
}

// jailWatch notifies about each workstream's memory: a process killed at
// its limit (memory.events oom_kill rising), and use reaching WarnAt of the
// limit (once; again only after it has fallen below Rearm; a kill is that
// warning too, as the kill says it reached the limit). Counts first
// seen notify nothing, so restarting the daemon replays no old kill; an
// unreadable slice changes nothing (its zeros would make the next good read
// replay every kill so far).
func (m *Machine) jailWatch(ws []core.JailWorkstream) []Effect {
	if !m.cfg.Notify || len(m.cfg.NotifyCommand) == 0 {
		return nil
	}
	notify := func(key, title, body string) []Effect {
		argv := append(append([]string{}, m.cfg.NotifyCommand...), title, body)
		return []Effect{Log{Msg: "jail: " + title}, Notify{Key: key, Argv: argv}}
	}
	var effs []Effect
	for _, w := range ws {
		st := w.Stats
		if st.Unreadable {
			continue
		}
		n := st.OOMKills
		prev, seen := m.js.oomKills[w.Slice]
		m.js.oomKills[w.Slice] = n
		if seen && n > prev {
			effs = append(effs, notify("jail:"+w.Name, "cccp: "+w.Name+" hit its memory limit",
				fmt.Sprintf("the kernel killed %d process(es) in it at %s", n-prev, present.Bytes(st.Max)))...)
			m.js.warned[w.Slice] = true
		}
		if st.Max <= 0 {
			continue
		}
		share := float64(st.Memory) / float64(st.Max)
		switch {
		case share >= jail.WarnAt && !m.js.warned[w.Slice]:
			m.js.warned[w.Slice] = true
			effs = append(effs, notify("jail:"+w.Name, fmt.Sprintf("cccp: %s is at %.0f%% of its memory limit", w.Name, 100*share),
				fmt.Sprintf("%s of %s; at the limit the kernel kills a process in it. Boost it with L in the cockpit.",
					present.Bytes(st.Memory), present.Bytes(st.Max)))...)
		case share < jail.Rearm:
			delete(m.js.warned, w.Slice)
		}
	}
	return effs
}

// jailStatsRead checks fresh numbers against the last snapshot's
// workstreams (none while jails are off).
func (m *Machine) jailStatsRead(in JailStatsRead) []Effect {
	ws := make([]core.JailWorkstream, 0, len(m.snap.Jail.Workstreams))
	for _, w := range m.snap.Jail.Workstreams {
		if st, ok := in.Stats[w.Slice]; ok {
			w.Stats = st
		}
		ws = append(ws, w)
	}
	return m.jailWatch(ws)
}
