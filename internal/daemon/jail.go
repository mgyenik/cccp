package daemon

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"cccp/internal/ipc"
	"cccp/internal/jail"
	"cccp/internal/machine"
)

// boostRequest turns an OpLimits request into the machine's input: preset
// "normal" ends a boost; another preset is a boost for For ("" until
// undone).
func boostRequest(r ipc.Request, now time.Time) (machine.SetBoost, error) {
	if r.Key == "" {
		return machine.SetBoost{}, errors.New("limits: no workstream")
	}
	in := machine.SetBoost{Workstream: r.Key, Preset: r.Value, At: now}
	switch r.Value {
	case jail.PresetNormal:
		in.Clear = true
		return in, nil
	case jail.PresetBoost, jail.PresetMax:
	default:
		return in, fmt.Errorf("limits: preset %q: the presets are %v", r.Value, jail.Presets)
	}
	if r.For != "" {
		d, err := time.ParseDuration(r.For)
		if err != nil || d <= 0 {
			return in, fmt.Errorf("limits: %q is not a length of time", r.For)
		}
		in.For = d
	}
	return in, nil
}

// jailLimits applies a slice's limits; a failure goes back to the machine,
// which retries later.
func (d *Daemon) jailLimits(e machine.JailLimits) {
	j := d.ports.Jail
	if j == nil {
		return
	}
	if err := j.SetLimits(e.Unit, e.Limits); err != nil {
		d.exec(d.m.Step(machine.JailDone{Unit: e.Unit, Err: err, At: d.ports.Now()}))
		return
	}
	d.reobserve = true // the slice's new numbers (its limits, as the kernel has them)
}

// moveRounds bounds the moving of a pane's processes: processes started
// while it is moved are moved after it, until its old cgroup is empty.
const moveRounds = 20

// jailMove moves a pane's processes (or the tmux server) into a new scope
// in its slice; a failure goes back to the machine.
func (d *Daemon) jailMove(m jail.Move) {
	j := d.ports.Jail
	if j == nil {
		return
	}
	if err := d.move(j, m); err != nil {
		d.exec(d.m.Step(machine.JailDone{Pane: m.Pane, From: m.From, Err: err, At: d.ports.Now()}))
		return
	}
	d.reobserve = true
}

func (d *Daemon) move(j Jailer, m jail.Move) error {
	var pids []int
	scope := jail.TmuxScope
	switch {
	case m.Pane == "":
		pids = []int{m.PID} // the server alone: its panes are placed one by one
	case m.Whole:
		pids = j.Procs(m.From)
	default:
		pids = j.Descendants(m.PID)
	}
	if m.Pane != "" {
		scope = fmt.Sprintf("cccp-pane-%s-%d.scope", strings.TrimPrefix(m.Pane, "%"), d.ports.Now().UnixNano())
	}
	if len(pids) == 0 {
		return nil // gone meanwhile
	}
	if err := j.Move(scope, m.Slice, pids); err != nil {
		return err
	}
	if !m.Whole {
		// systemd moves them when its job runs: wait (briefly) until it has,
		// so the next look sees them where they now are.
		for i := 0; i < moveRounds*2 && !strings.HasSuffix(j.Cgroup(pids[0]), "/"+scope); i++ {
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	}
	// The scope is created asynchronously; processes forked meanwhile are
	// still in the old cgroup. Attach them until it is empty.
	for i := 0; i < moveRounds; i++ {
		rest := j.Procs(m.From)
		if len(rest) == 0 {
			return nil
		}
		_ = j.Attach(scope, rest) // fails until the scope is up: try again
		time.Sleep(5 * time.Millisecond)
	}
	if rest := j.Procs(m.From); len(rest) > 0 {
		return fmt.Errorf("pane %s: %d processes still in %s", m.Pane, len(rest), m.From)
	}
	return nil
}

// JailPoll is how often the jailed workstreams' numbers are read between
// observations.
const JailPoll = 15 * time.Second

// pollJail reads the running workstreams' slices and lets the machine warn
// about any near its limit (while jails are off the snapshot has none).
func (d *Daemon) pollJail() {
	snap, ok := d.m.Snapshot()
	if !ok || d.ports.Jail == nil {
		return
	}
	stats := map[string]jail.Stats{}
	for _, w := range snap.Jail.Workstreams {
		stats[w.Slice] = d.ports.Jail.Stats(w.Slice)
	}
	d.exec(d.m.Step(machine.JailStatsRead{Stats: stats, At: d.ports.Now()}))
}
