package machine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"cccp/internal/core"
	"cccp/internal/jail"
	"cccp/internal/model"
)

const userRoot = "/user.slice/user-1000.slice/user@1000.service"

// jailScene is tmux with two workstreams: infra in place, docs a new pane
// still in A.
func jailScene(oomKills int64) core.Observation {
	return core.Observation{
		Profiles: testProfiles,
		Tmux: core.TmuxView{OK: true, ServerPID: 100, Panes: []model.TmuxPane{
			{ID: "%1", PID: 101, Session: "infra", WindowID: "@1"},
			{ID: "%2", PID: 102, Session: "docs", WindowID: "@2"},
		}},
		Jail: core.JailView{Available: true, ServerCgroup: userRoot + "/cccp.slice/cccp-tmux.scope",
			PaneCgroups: map[string]string{
				"%1": userRoot + "/cccp.slice/cccp-ws.slice/cccp-ws-infra.slice/cccp-pane-1.scope",
				"%2": userRoot + "/cccp.slice/tmux-spawn-x.scope",
			},
			Stats: map[string]jail.Stats{"cccp-ws-infra.slice": {OOMKills: oomKills}}},
	}
}

// placedScene is jailScene with every pane already in its workstream.
func placedScene() core.Observation {
	o := jailScene(0)
	o.Jail.PaneCgroups["%2"] = userRoot + "/cccp.slice/cccp-ws.slice/cccp-ws-docs.slice/cccp-pane-2.scope"
	return o
}

func jailEffects(effs []Effect) []string {
	var out []string
	for _, e := range effs {
		switch e := e.(type) {
		case JailLimits:
			out = append(out, "limits "+e.Unit)
		case JailMove:
			out = append(out, "move "+e.Move.Pane+" → "+e.Move.Slice)
		}
	}
	return out
}

func TestJailAppliesLimitsThenMoves(t *testing.T) {
	m := newMachine(testConfig())
	got := jailEffects(m.Step(Observed{Obs: jailScene(0), At: at(1)}))
	want := "limits cccp.slice|limits cccp-ws-docs.slice|limits cccp-ws-infra.slice|move %2 → cccp-ws-docs.slice"
	if strings.Join(got, "|") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, "|"), want)
	}
	// The same state again: nothing to apply, the move not repeated.
	if got := jailEffects(m.Step(Observed{Obs: jailScene(0), At: at(2)})); len(got) != 0 {
		t.Fatalf("repeated: %v", got)
	}
	// Still not moved a minute later: tried again, its slice's limits first.
	got = jailEffects(m.Step(TimerFired{At: at(1).Add(JailRetry)}))
	if strings.Join(got, "|") != "limits cccp.slice|limits cccp-ws-docs.slice|move %2 → cccp-ws-docs.slice" {
		t.Fatalf("retry: %v", got)
	}
}

func TestJailFailureIsRetriedLater(t *testing.T) {
	m := newMachine(testConfig())
	m.Step(Observed{Obs: placedScene(), At: at(1)})
	effs := m.Step(JailDone{Unit: "cccp-ws-infra.slice", Err: errors.New("dbus said no"), At: at(2)})
	if len(of[Log](effs)) != 1 {
		t.Fatalf("%v", effs)
	}
	// The retry is a deadline: once the pending save (due first) is done,
	// the timer is set for it.
	if a := of[ArmTimer](m.Step(TimerFired{At: at(1).Add(SaveDelay)})); len(a) != 1 || !a[0].At.Equal(at(2).Add(JailRetry)) {
		t.Fatalf("timer %v, want the retry at %v", a, at(2).Add(JailRetry))
	}
	if got := jailEffects(m.Step(Observed{Obs: placedScene(), At: at(3)})); len(got) != 0 {
		t.Fatalf("retried before its time: %v", got)
	}
	if got := jailEffects(m.Step(TimerFired{At: at(2).Add(JailRetry)})); strings.Join(got, "|") != "limits cccp-ws-infra.slice" {
		t.Fatalf("retry: %v", got)
	}
}

func TestBoostAppliesAndPersists(t *testing.T) {
	m := newMachine(testConfig())
	m.Step(Observed{Obs: jailScene(0), At: at(1)})
	effs := m.Step(SetBoost{Workstream: "infra", Preset: jail.PresetMax, For: time.Hour, At: at(2)})
	got := strings.Join(jailEffects(effs), "|")
	if got != "limits cccp.slice|limits cccp-ws-infra.slice" {
		t.Fatalf("max lifts A and the workstream: %s", got)
	}
	for _, e := range effs {
		if l, ok := e.(JailLimits); ok && l.Unit == jail.Root && l.Limits != jail.Defaults.RootMax {
			t.Fatalf("A at %+v", l.Limits)
		}
	}
	if b := m.Memory().Boosts["infra"]; b.Preset != jail.PresetMax || !b.Until.Equal(at(2).Add(time.Hour)) {
		t.Fatalf("boost %+v", b)
	}
	m.Step(SetBoost{Workstream: "infra", Clear: true, At: at(3)})
	if _, ok := m.Memory().Boosts["infra"]; ok {
		t.Fatal("not cleared")
	}
}

func TestKillAtTheLimitNotifiesOnce(t *testing.T) {
	m := newMachine(testConfig())
	if n := of[Notify](m.Step(Observed{Obs: jailScene(3), At: at(1)})); len(n) != 0 {
		t.Fatal("kills before watching began notified")
	}
	n := of[Notify](m.Step(Observed{Obs: jailScene(4), At: at(2)}))
	if len(n) != 1 || !strings.Contains(strings.Join(n[0].Argv, " "), "infra hit its memory limit") {
		t.Fatalf("%v", n)
	}
	if n := of[Notify](m.Step(Observed{Obs: jailScene(4), At: at(3)})); len(n) != 0 {
		t.Fatal("notified twice")
	}
	// The slice made again (after a reboot, say) starts counting at 0: no
	// notification for that, and a kill from there is one kill.
	if n := of[Notify](m.Step(Observed{Obs: jailScene(0), At: at(4)})); len(n) != 0 {
		t.Fatalf("counter restarted: %v", n)
	}
	n = of[Notify](m.Step(Observed{Obs: jailScene(1), At: at(5)}))
	if len(n) != 1 || !strings.Contains(strings.Join(n[0].Argv, " "), "killed 1 process") {
		t.Fatalf("%v", n)
	}
}

func TestJailOffDoesNothing(t *testing.T) {
	c := testConfig()
	c.Jail.Enabled = false
	m := newMachine(c)
	if got := jailEffects(m.Step(Observed{Obs: jailScene(0), At: at(1)})); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

// near is placedScene with infra using memory of a 100-byte limit.
func near(memory int64) core.Observation {
	o := placedScene()
	o.Jail.Stats["cccp-ws-infra.slice"] = jail.Stats{Memory: memory, Max: 100}
	return o
}

func notified(effs []Effect) []string {
	var out []string
	for _, n := range of[Notify](effs) {
		out = append(out, n.Argv[len(n.Argv)-2])
	}
	return out
}

func TestNearTheLimitWarnsOncePerCrossing(t *testing.T) {
	m := newMachine(testConfig())
	if n := notified(m.Step(Observed{Obs: near(84), At: at(1)})); len(n) != 0 {
		t.Fatalf("84%%: %v", n)
	}
	if n := notified(m.Step(Observed{Obs: near(85), At: at(2)})); len(n) != 1 || n[0] != "cccp: infra is at 85% of its memory limit" {
		t.Fatalf("85%%: %v", n)
	}
	// Still near, or dipping but not below Rearm: no repeat.
	for i, mem := range []int64{95, 76, 75, 90} {
		if n := notified(m.Step(Observed{Obs: near(mem), At: at(3 + i)})); len(n) != 0 {
			t.Fatalf("%d%%: repeated %v", mem, n)
		}
	}
	// Below Rearm, then near again: warned again.
	m.Step(Observed{Obs: near(74), At: at(10)})
	if n := notified(m.Step(Observed{Obs: near(86), At: at(11)})); len(n) != 1 {
		t.Fatalf("after re-arming: %v", n)
	}
	// The same through observations: an unreadable slice in a snapshot
	// does not re-arm the warning.
	unread := near(0)
	unread.Jail.Stats["cccp-ws-infra.slice"] = jail.Stats{Unreadable: true}
	m.Step(Observed{Obs: unread, At: at(12)})
	if n := notified(m.Step(Observed{Obs: near(86), At: at(13)})); len(n) != 0 {
		t.Fatalf("an unreadable read re-armed: %v", n)
	}
	// A kill reported with use still near the limit: the kill alone (it
	// says the limit was reached), and no warning until it has re-armed.
	m.Step(Observed{Obs: near(10), At: at(15)})
	kill := near(99)
	kill.Jail.Stats["cccp-ws-infra.slice"] = jail.Stats{Memory: 99, Max: 100, OOMKills: 1}
	if n := notified(m.Step(Observed{Obs: kill, At: at(16)})); len(n) != 1 || !strings.Contains(n[0], "hit its memory limit") {
		t.Fatalf("kill near the limit: %v", n)
	}
	// No limit: never near it.
	o := placedScene()
	o.Jail.Stats["cccp-ws-docs.slice"] = jail.Stats{Memory: 1 << 40}
	if n := notified(m.Step(Observed{Obs: o, At: at(14)})); len(n) != 0 {
		t.Fatalf("no limit: %v", n)
	}
}

// The daemon's poll reads numbers between observations: they are checked
// against the last snapshot's workstreams.
func TestPolledNumbersWarn(t *testing.T) {
	m := newMachine(testConfig())
	m.Step(Observed{Obs: near(10), At: at(1)})
	poll := func(st jail.Stats, sec int) []string {
		return notified(m.Step(JailStatsRead{Stats: map[string]jail.Stats{"cccp-ws-infra.slice": st}, At: at(sec)}))
	}
	if n := poll(jail.Stats{Memory: 90, Max: 100}, 2); len(n) != 1 || !strings.Contains(n[0], "90%") {
		t.Fatalf("%v", n)
	}
	if n := poll(jail.Stats{Memory: 60, Max: 100, OOMKills: 1}, 3); len(n) != 1 || !strings.Contains(n[0], "hit its memory limit") {
		t.Fatalf("kill seen by the poll: %v", n)
	}
	// An unreadable read changes nothing: the kill already reported is not
	// reported again after it.
	if n := poll(jail.Stats{Unreadable: true}, 4); len(n) != 0 {
		t.Fatalf("%v", n)
	}
	if n := poll(jail.Stats{Memory: 60, Max: 100, OOMKills: 1}, 5); len(n) != 0 {
		t.Fatalf("replayed after an unreadable read: %v", n)
	}
	// Jails off: the poll does nothing.
	c := testConfig()
	c.Jail.Enabled = false
	off := newMachine(c)
	off.Step(Observed{Obs: near(10), At: at(1)})
	if n := notified(off.Step(JailStatsRead{Stats: map[string]jail.Stats{"cccp-ws-infra.slice": {Memory: 99, Max: 100}}, At: at(2)})); len(n) != 0 {
		t.Fatalf("off: %v", n)
	}
}
