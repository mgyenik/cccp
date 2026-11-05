package jail

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func TestEscapeKeepsNamesOneLevel(t *testing.T) {
	for in, want := range map[string]string{
		"infra":         "infra",
		"work/infra":    `work\x2finfra`, // not "work-infra": a dash is a level
		"my-proj":       `my\x2dproj`,
		"two words":     `two\x20words`,
		".hidden":       `\x2ehidden`,
		"v2_final:test": "v2_final:test",
	} {
		if got := Escape(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
	if SliceFor("my-proj") != `cccp-ws-my\x2dproj.slice` {
		t.Fatal(SliceFor("my-proj"))
	}
}

func TestEffective(t *testing.T) {
	s := Defaults
	s.Workstreams = map[string]WorkstreamSetting{
		"heavy": {Preset: PresetBoost},
		"tuned": {Custom: Limits{MemoryHigh: "50%"}},
	}
	boosts := map[string]Boost{"infra": {Preset: PresetMax, Until: t0.Add(time.Hour)}, "old": {Preset: PresetBoost, Until: t0.Add(-time.Minute)}}
	cases := map[string]string{"infra": PresetMax, "old": PresetNormal, "heavy": PresetBoost, "plain": PresetNormal, "tuned": "custom"}
	for ws, want := range cases {
		if _, got := s.Effective(ws, boosts, t0); got != want {
			t.Errorf("%s: %s want %s", ws, got, want)
		}
	}
	if l, _ := s.Effective("tuned", nil, t0); l.MemoryHigh != "50%" || l.MemoryMax != Defaults.Presets[PresetNormal].MemoryMax {
		t.Fatalf("custom over normal: %+v", l)
	}
	// An unknown preset in a boost or the config falls back to normal.
	if _, got := s.Effective("x", map[string]Boost{"x": {Preset: "turbo"}}, t0); got != PresetNormal {
		t.Fatal(got)
	}
}

func TestDesiredRaisesTheRootOnlyForMax(t *testing.T) {
	s := Defaults
	d := s.Desired([]string{"a", "b"}, map[string]Boost{"a": {Preset: PresetBoost}}, t0)
	if d[Root] != s.Root || d[SliceFor("a")] != s.Presets[PresetBoost] || d[SliceFor("b")] != s.Presets[PresetNormal] {
		t.Fatalf("%+v", d)
	}
	d = s.Desired([]string{"a", "b"}, map[string]Boost{"b": {Preset: PresetMax}}, t0)
	if d[Root] != s.RootMax {
		t.Fatalf("max lifts A: %+v", d[Root])
	}
	if len(d) != 3 {
		t.Fatalf("units %v", d)
	}
}

func TestMergeEachField(t *testing.T) {
	base := Limits{MemoryHigh: "1", MemoryMax: "2", MemorySwapMax: "3", CPUWeight: 4, TasksMax: 5}
	all := Limits{MemoryHigh: "a", MemoryMax: "b", MemorySwapMax: "c", CPUWeight: 6, TasksMax: 7}
	if got := base.Merge(all); got != all {
		t.Fatalf("every field set: %+v", got)
	}
	if got := base.Merge(Limits{}); got != base {
		t.Fatalf("none set: %+v", got)
	}
}

func TestBoostExpiry(t *testing.T) {
	boosts := map[string]Boost{
		"a": {Preset: PresetBoost, Until: t0.Add(time.Hour)},
		"b": {Preset: PresetBoost, Until: t0.Add(2 * time.Hour)},
		"c": {Preset: PresetMax},                                // until undone
		"d": {Preset: PresetBoost, Until: t0.Add(-time.Second)}, // over
	}
	if got := Expire(boosts, t0); len(got) != 3 || got["d"] != (Boost{}) {
		t.Fatalf("%+v", got)
	}
	if !Deadline(boosts, t0).Equal(t0.Add(time.Hour)) {
		t.Fatal(Deadline(boosts, t0))
	}
	if !Deadline(map[string]Boost{"c": {Preset: PresetMax}}, t0).IsZero() {
		t.Fatal("until undone has no deadline")
	}
	if !(Boost{Until: t0}).Active(t0.Add(-time.Nanosecond)) || (Boost{Until: t0}).Active(t0) {
		t.Fatal("active until, not at, its end")
	}
}

func TestPlacement(t *testing.T) {
	base := "/user.slice/user-1000.slice/user@1000.service"
	server := base + "/app.slice/vte-spawn-y.scope"
	panes := []Pane{
		{ID: "%1", PID: 11, Workstream: "infra", Cgroup: base + "/cccp.slice/cccp-ws.slice/cccp-ws-infra.slice/cccp-pane-1.scope"}, // in place
		{ID: "%2", PID: 12, Workstream: "infra", Cgroup: base + "/cccp.slice/tmux-spawn-x.scope"},                                  // new pane
		{ID: "%3", PID: 13, Workstream: "docs", Cgroup: base + "/cccp.slice/cccp-ws.slice/cccp-ws-infra.slice/cccp-pane-3.scope"},  // moved with M
		{ID: "%4", PID: 14, Workstream: "docs"},  // cgroup unknown: left alone
		{ID: "%5", PID: 15, Cgroup: base + "/x"}, // no workstream: left alone
	}
	got := Placement(100, server, panes)
	want := []Move{
		{PID: 100, From: server, Slice: Root},
		{Pane: "%2", PID: 12, From: base + "/cccp.slice/tmux-spawn-x.scope", Slice: "cccp-ws-infra.slice", Whole: true},
		{Pane: "%3", PID: 13, From: base + "/cccp.slice/cccp-ws.slice/cccp-ws-infra.slice/cccp-pane-3.scope", Slice: "cccp-ws-docs.slice", Whole: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	// The server in place, and a workstream whose name another one's
	// starts with: in place means exactly its slice.
	got = Placement(100, base+"/cccp.slice/cccp-tmux.scope", []Pane{{ID: "%1", Workstream: "inf", Cgroup: base + "/cccp.slice/cccp-ws.slice/cccp-ws-infra.slice/s.scope"}})
	if len(got) != 1 || got[0].Slice != "cccp-ws-inf.slice" {
		t.Fatalf("%+v", got)
	}
}

// tmux without systemd support keeps every pane in the server's cgroup:
// then only each pane's own processes move, never the whole cgroup.
func TestPlacementNeverMovesASharedCgroupWhole(t *testing.T) {
	shared := "/user.slice/user-1000.slice/user@1000.service/app.slice/tmux.scope"
	got := Placement(100, shared, []Pane{
		{ID: "%1", PID: 11, Workstream: "a", Cgroup: shared},
		{ID: "%2", PID: 12, Workstream: "b", Cgroup: shared},
	})
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	for _, m := range got {
		if m.Whole {
			t.Fatalf("moved a shared cgroup whole: %+v", m)
		}
	}
	// One pane sharing the server's cgroup: still shared (moving it whole
	// would take the server along).
	got = Placement(100, shared, []Pane{{ID: "%1", PID: 11, Workstream: "a", Cgroup: shared}})
	if len(got) != 2 || got[1].Whole {
		t.Fatalf("%+v", got)
	}
}

func TestParse(t *testing.T) {
	ev := ParseKeyed("low 0\nhigh 12\nmax 3\noom 1\noom_kill 1\n")
	if ev["high"] != 12 || ev["oom_kill"] != 1 {
		t.Fatal(ev)
	}
	if ParseBytes("4620484608\n") != 4620484608 || ParseBytes("max\n") != 0 {
		t.Fatal("bytes")
	}
}

// In place means the workstream's slice is a component of the path, not a
// substring of one.
func TestPlacementMatchesWholeComponents(t *testing.T) {
	got := Placement(0, "", []Pane{{ID: "%1", Workstream: "a", Cgroup: "/u/not-cccp-ws-a.slice/x.scope"}})
	if len(got) != 1 || got[0].Slice != "cccp-ws-a.slice" {
		t.Fatalf("%+v", got)
	}
}
