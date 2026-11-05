package daemon

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cccp/internal/config"
	"cccp/internal/core"
	"cccp/internal/ipc"
	"cccp/internal/jail"
	"cccp/internal/machine"
	"cccp/internal/model"
)

func TestBoostRequest(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	in, err := boostRequest(ipc.Request{Key: "infra", Value: jail.PresetBoost, For: "2h"}, now)
	if err != nil || in.Workstream != "infra" || in.Preset != jail.PresetBoost || in.For != 2*time.Hour || in.Clear {
		t.Fatalf("%+v %v", in, err)
	}
	if in, _ := boostRequest(ipc.Request{Key: "infra", Value: jail.PresetMax}, now); in.For != 0 {
		t.Fatal("no length: until undone")
	}
	if in, _ := boostRequest(ipc.Request{Key: "infra", Value: jail.PresetNormal}, now); !in.Clear {
		t.Fatal("normal ends the boost")
	}
	for _, bad := range []ipc.Request{{Value: jail.PresetBoost}, {Key: "x", Value: "turbo"}, {Key: "x", Value: jail.PresetBoost, For: "soon"}, {Key: "x", Value: jail.PresetBoost, For: "-1h"}} {
		if _, err := boostRequest(bad, now); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// fakeJailer records moves; procs is each cgroup's processes, and arrivals
// are processes that appear in a cgroup after each read (forked mid-move).
type fakeJailer struct {
	procs    map[string][]int
	arrivals map[string][][]int
	tree     map[int][]int
	moved    []string
	failMove error
	cgroup   string // where any process now is
	stats    map[string]jail.Stats
	read     []string // slices whose numbers were read
	events   string   // directory of the slices' memory.events ("": none)
}

func (f *fakeJailer) SetLimits(string, jail.Limits) error { return nil }
func (f *fakeJailer) EventsFile(s string) string {
	if f.events != "" {
		return filepath.Join(f.events, s)
	}
	return "/x/" + s
}
func (f *fakeJailer) Descendants(pid int) []int { return append([]int{pid}, f.tree[pid]...) }
func (f *fakeJailer) Cgroup(pid int) string     { return f.cgroup }
func (f *fakeJailer) Stats(s string) jail.Stats {
	f.read = append(f.read, s)
	return f.stats[s]
}
func (f *fakeJailer) Procs(cg string) []int {
	out := f.procs[cg]
	if a := f.arrivals[cg]; len(a) > 0 {
		f.procs[cg], f.arrivals[cg] = a[0], a[1:]
	} else {
		f.procs[cg] = nil
	}
	return out
}
func (f *fakeJailer) Move(scope, slice string, pids []int) error {
	if f.failMove != nil {
		return f.failMove
	}
	f.moved = append(f.moved, strings.Join([]string{scopeKind(scope), slice, ints(pids)}, " "))
	return nil
}
func (f *fakeJailer) Attach(unit string, pids []int) error {
	f.moved = append(f.moved, "attach "+scopeKind(unit)+" "+ints(pids))
	return nil
}

func scopeKind(s string) string {
	if strings.HasPrefix(s, "cccp-pane-") {
		return "pane-scope"
	}
	return s
}

func ints(xs []int) string {
	var s []string
	for _, x := range xs {
		s = append(s, string(rune('0'+x%10)))
	}
	return strings.Join(s, ",")
}

func TestMovePaneWholeAttachesStragglers(t *testing.T) {
	d := &Daemon{ports: Ports{Now: time.Now}}
	f := &fakeJailer{procs: map[string][]int{"/old": {1, 2}}, arrivals: map[string][][]int{"/old": {{3}}}}
	if err := d.move(f, jail.Move{Pane: "%4", PID: 1, From: "/old", Slice: "cccp-ws-a.slice", Whole: true}); err != nil {
		t.Fatal(err)
	}
	want := []string{"pane-scope cccp-ws-a.slice 1,2", "attach pane-scope 3"}
	if !reflect.DeepEqual(f.moved, want) {
		t.Fatalf("%v", f.moved)
	}
}

func TestMoveSharedCgroupMovesOnlyThePanesTree(t *testing.T) {
	d := &Daemon{ports: Ports{Now: time.Now}}
	f := &fakeJailer{procs: map[string][]int{"/shared": {1, 2, 5, 6, 7}}, tree: map[int][]int{5: {6}}}
	if err := d.move(f, jail.Move{Pane: "%2", PID: 5, From: "/shared", Slice: "cccp-ws-b.slice"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.moved, []string{"pane-scope cccp-ws-b.slice 5,6"}) {
		t.Fatalf("%v", f.moved)
	}
}

func TestMoveServerAlone(t *testing.T) {
	d := &Daemon{ports: Ports{Now: time.Now}}
	f := &fakeJailer{procs: map[string][]int{"/vte": {8, 9}}}
	if err := d.move(f, jail.Move{PID: 9, From: "/vte", Slice: jail.Root}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.moved, []string{"cccp-tmux.scope cccp.slice 9"}) {
		t.Fatalf("%v", f.moved)
	}
}

func TestMoveThatNeverEmptiesFails(t *testing.T) {
	d := &Daemon{ports: Ports{Now: time.Now}}
	stuck := make([][]int, moveRounds+5)
	for i := range stuck {
		stuck[i] = []int{7}
	}
	f := &fakeJailer{procs: map[string][]int{"/old": {1}}, arrivals: map[string][][]int{"/old": stuck}}
	if err := d.move(f, jail.Move{Pane: "%1", PID: 1, From: "/old", Slice: "s.slice", Whole: true}); err == nil || !strings.Contains(err.Error(), "still in") {
		t.Fatalf("%v", err)
	}
	f = &fakeJailer{procs: map[string][]int{"/old": {1}}, failMove: errors.New("no")}
	if err := d.move(f, jail.Move{Pane: "%1", PID: 1, From: "/old", Slice: "s.slice", Whole: true}); err == nil {
		t.Fatal("a failed move is an error")
	}
}

// The poll reads each running workstream's slice and warns through the
// machine; with jails off it reads nothing.
func TestPollJailWarnsNearTheLimit(t *testing.T) {
	for _, on := range []bool{true, false} {
		cfg := config.Default()
		cfg.NotifyCommand = []string{"notify"}
		cfg.Jail.Enabled = on
		mem := core.NewMemory()
		mem.Initialized = true
		m := machine.New(cfg, mem, time.Unix(0, 0))
		f := &fakes{}
		jl := &fakeJailer{stats: map[string]jail.Stats{"cccp-ws-infra.slice": {Memory: 90, Max: 100}}}
		d := New(Options{}, Ports{Jail: jl, Notifier: f, Log: log.New(f, "", 0), Now: time.Now}, m)
		root := "/user.slice/user-1000.slice/user@1000.service/cccp.slice"
		m.Step(machine.Observed{At: time.Unix(1, 0), Obs: core.Observation{
			Tmux: core.TmuxView{OK: true, ServerPID: 1, Panes: []model.TmuxPane{{ID: "%1", PID: 2, Session: "infra", WindowID: "@1"}}},
			Jail: core.JailView{Available: true, ServerCgroup: root + "/cccp-tmux.scope",
				PaneCgroups: map[string]string{"%1": root + "/cccp-ws.slice/cccp-ws-infra.slice/p.scope"},
				Stats:       map[string]jail.Stats{"cccp-ws-infra.slice": {Memory: 10, Max: 100}}},
		}})
		d.pollJail()
		switch {
		case on && (!reflect.DeepEqual(jl.read, []string{"cccp-ws-infra.slice"}) || len(f.notified) != 1 || !strings.Contains(strings.Join(f.notified[0], " "), "90%")):
			t.Fatalf("on: read %v, notified %v", jl.read, f.notified)
		case !on && (len(jl.read) != 0 || len(f.notified) != 0):
			t.Fatalf("off: read %v, notified %v", jl.read, f.notified)
		}
	}
}

// A slice's memory.events is watched once it exists: systemd makes the
// slice after its limits are set, so a watch that fails at first is tried
// again at each observation, and a change to the file is then observed.
func TestSliceEventsAreWatchedOnceTheSliceExists(t *testing.T) {
	events := t.TempDir()
	root := "/user.slice/user-1000.slice/user@1000.service/cccp.slice"
	h := start(t, testCfg(), func(f *fakes, p *Ports) {
		f.jail = core.JailView{Available: true, ServerCgroup: root + "/cccp-tmux.scope",
			PaneCgroups: map[string]string{"%1": root + "/cccp-ws.slice/cccp-ws-s.slice/p.scope"}}
		p.Jail = &fakeJailer{events: events}
	})
	file := filepath.Join(events, "cccp-ws-s.slice")
	if err := os.WriteFile(file, []byte("oom_kill 0\n"), 0o644); err != nil { // the slice now exists
		t.Fatal(err)
	}
	if _, err := h.client.Call(ipc.Request{Op: ipc.OpRefresh}, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	before := 0
	h.f.get(func(f *fakes) bool { before = f.observes; return true })
	if err := os.WriteFile(file, []byte("oom_kill 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the change to the slice's memory.events observed", func() bool {
		return h.f.get(func(f *fakes) bool { return f.observes > before })
	})
}
