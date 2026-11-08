package core

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"cccp/internal/config"
	"cccp/internal/jail"
	"cccp/internal/model"
	"cccp/internal/workstream"
)

// --- fixtures ----------------------------------------------------------------

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

var cfg = config.Default()

// pane is a single-pane window in session "s".
func pane(id, window string, idx int) model.TmuxPane {
	return model.TmuxPane{ID: id, Session: "s", WindowID: window, WindowIndex: idx, WindowPanes: 1}
}

func hookRec(sid string, pid int, alive bool, f func(*model.HookState)) HookRecord {
	st := model.HookState{Version: 1, Agent: model.Claude, SessionID: sid, PID: pid, PIDStart: 1}
	if f != nil {
		f(&st)
	}
	return HookRecord{Path: "/state/" + sid + ".json", State: st, ProcAlive: alive}
}

func regRec(sid string, pid int, status string, statusAt time.Time, f func(*model.RegistryEntry)) RegistryRecord {
	e := model.RegistryEntry{PID: pid, SessionID: sid, Kind: "interactive", Entrypoint: "cli", Status: status,
		StatusUpdatedMs: statusAt.UnixMilli(), Cwd: "/w/" + sid, Name: sid + "-1", NameSource: "derived"}
	if f != nil {
		f(&e)
	}
	return RegistryRecord{Path: "/reg/" + sid, Entry: e, Alive: true}
}

func find(t *testing.T, s Snapshot, key string) Session {
	t.Helper()
	for _, x := range s.Sessions {
		if x.Key == key {
			return x
		}
	}
	t.Fatalf("no session %q in %v", key, keys(s))
	return Session{}
}

func absent(t *testing.T, s Snapshot, key string) {
	t.Helper()
	for _, x := range s.Sessions {
		if x.Key == key {
			t.Fatalf("session %q should not be listed: %+v", key, x)
		}
	}
}

func keys(s Snapshot) []string {
	var k []string
	for _, x := range s.Sessions {
		k = append(k, x.Key)
	}
	return k
}

// compute runs Plan-then-Compute like the shell does, with initialized
// memory (not a first run) unless mem is given.
func compute(obs Observation, mem *Memory, now time.Time) (Snapshot, Memory) {
	m := NewMemory()
	m.Initialized = true
	if mem != nil {
		m = *mem
	}
	return Compute(obs, m, cfg, t0, now)
}

// --- pure helpers ------------------------------------------------------------

func TestMergeActivity(t *testing.T) {
	hook := func(a model.Activity, sec int) *model.HookState {
		return &model.HookState{Activity: a, ActivityAt: at(sec), Tool: "Bash", ToolDetail: "make", Question: "q"}
	}
	reg := func(status, waiting string, sec int) *model.RegistryEntry {
		return &model.RegistryEntry{Status: status, WaitingFor: waiting, StatusUpdatedMs: at(sec).UnixMilli()}
	}
	cases := []struct {
		name     string
		h        *model.HookState
		r        *model.RegistryEntry
		want     model.Activity
		wantTool string
		since    time.Time
	}{
		{"hook only", hook(model.Working, 5), nil, model.Working, "Bash", at(5)},
		{"registry only", nil, reg("waiting", "permission prompt", 5), model.NeedsApproval, "", at(5)},
		{"same class keeps hook detail", hook(model.NeedsApproval, 5), reg("waiting", "permission prompt", 6), model.NeedsApproval, "Bash", at(5)},
		{"same class, registry first: activity began then", hook(model.Working, 5), reg("busy", "", 2), model.Working, "Bash", at(2)},
		{"errored refines idle", hook(model.Errored, 5), reg("idle", "", 6), model.Errored, "Bash", at(5)},
		{"interrupt: newer registry idle wins", hook(model.Working, 5), reg("idle", "", 9), model.Idle, "", at(9)},
		{"manual deny: newer registry busy wins", hook(model.NeedsApproval, 5), reg("busy", "", 9), model.Working, "", at(9)},
		{"older registry loses", hook(model.Working, 9), reg("idle", "", 5), model.Working, "Bash", at(9)},
		{"unknown registry status, newer: the hook stands", hook(model.Idle, 5), reg("weird", "", 9), model.Idle, "Bash", at(5)},
		{"unknown registry status alone", nil, reg("weird", "", 9), model.Unknown, "", at(9)},
		{"shell: idle with a background shell running", hook(model.Working, 5), reg("shell", "", 9), model.Idle, "", at(9)},
		{"neither", nil, nil, model.Unknown, "", time.Time{}},
	}
	for _, c := range cases {
		var s Session
		mergeActivity(&s, c.h, c.r, model.ClaudeMeta{})
		if s.Activity != c.want || s.Tool != c.wantTool || !s.ActivitySince.Equal(c.since) {
			t.Errorf("%s: got %s/%q since %v, want %s/%q since %v", c.name, s.Activity, s.Tool, s.ActivitySince, c.want, c.wantTool, c.since)
		}
	}
	// A registry-only approval wait names the pending tool from the transcript.
	var s Session
	mergeActivity(&s, nil, reg("waiting", "permission prompt", 5), model.ClaudeMeta{PendingToolName: "Edit"})
	if s.Tool != "Edit" || s.Question != "permission prompt" {
		t.Fatalf("%+v", s)
	}
}

func TestGroupOf(t *testing.T) {
	warm := Cache{Known: true, ExpiresAt: at(60)}
	cold := Cache{Known: true, ExpiresAt: at(-60)}
	cases := []struct {
		s    Session
		want Group
	}{
		{Session{Alive: true, Activity: model.NeedsApproval}, GroupNeedsYou},
		{Session{Alive: true, Activity: model.NeedsInput}, GroupNeedsYou},
		{Session{Alive: true, Activity: model.Errored}, GroupNeedsYou},
		{Session{Alive: true, Activity: model.Working}, GroupWorking},
		{Session{Alive: true, Activity: model.Idle, Unseen: true, Agent: model.Claude, Cache: cold}, GroupYourTurn},
		{Session{Alive: true, Activity: model.Idle, Agent: model.Claude, Cache: warm}, GroupIdle},
		{Session{Alive: true, Activity: model.Idle, Agent: model.Claude, Cache: cold}, GroupCold},
		{Session{Alive: true, Activity: model.Idle, Agent: model.Claude}, GroupIdle},
		{Session{Alive: true, Activity: model.Idle, Agent: model.Agy, Cache: cold}, GroupCold}, // an estimated agy cache past its cold point
		{Session{Alive: true, Activity: model.Idle, Agent: model.Agy}, GroupIdle},              // no estimate: idle
		{Session{Alive: true, Activity: model.Unknown}, GroupUnknown},
		{Session{Alive: false, Activity: model.Working}, GroupClosed},
		{Session{Alive: true, Activity: model.Ended}, GroupClosed},
	}
	for i, c := range cases {
		if g := groupOf(c.s, t0); g != c.want {
			t.Errorf("case %d: got %s want %s", i, g, c.want)
		}
	}
}

func TestCleanPaneTitle(t *testing.T) {
	for in, want := range map[string]string{
		"✳ Claude Code control panel design": "Claude Code control panel design",
		"  plain title ":                     "plain title",
		"laptop":                             "laptop",
		"✳":                                  "✳",
		"":                                   "",
	} {
		if got := CleanPaneTitle(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestFlagValue(t *testing.T) {
	args := []string{"claude", "--model", "x", "--resume", "abc", "--conversation=c1"}
	if v := flagValue(args, "--resume", "-r"); v != "abc" {
		t.Fatal(v)
	}
	if v := flagValue(args, "--conversation"); v != "c1" {
		t.Fatal(v)
	}
	if v := flagValue([]string{"claude", "--resume", "--verbose"}, "--resume"); v != "" {
		t.Fatalf("a following flag is not a value: %q", v)
	}
}

// --- Compute: hook sessions --------------------------------------------------

func TestHookSessionWorkingAndPane(t *testing.T) {
	obs := Observation{
		Hooks: []HookRecord{hookRec("s1", 42, true, func(s *model.HookState) {
			s.Pane, s.Activity, s.ActivityAt, s.Tool, s.ToolDetail = "%3", model.Working, at(1), "Bash", "make"
			s.Cwd, s.StartedAt, s.LastEventAt = "/w/proj", at(0), at(1)
		})},
		Tmux: TmuxView{OK: true, Panes: []model.TmuxPane{pane("%3", "@7", 2)}},
	}
	snap, _ := compute(obs, nil, at(5))
	s := find(t, snap, "claude:s1")
	if s.Group != GroupWorking || !s.Alive || s.Tracking != TrackHooks || s.Inferred {
		t.Fatalf("%+v", s)
	}
	if s.Pane != "%3" || s.WindowID != "@7" || s.Location != "s:2" || s.Visible {
		t.Fatalf("pane: %+v", s)
	}
	if s.Title != "proj" || s.TitleSource != "cwd" {
		t.Fatalf("title %q from %q", s.Title, s.TitleSource)
	}
}

// reporting is a client that attached with focus reporting on (its focus
// flag is real).
func reporting(name, pane string, focused bool) model.TmuxClient {
	return model.TmuxClient{Name: name, Pane: pane, Focused: focused, Created: t0, FocusSince: t0}
}

func yourTurnObs(focused bool) Observation {
	return Observation{
		Hooks: []HookRecord{hookRec("s1", 42, true, func(s *model.HookState) {
			s.Pane, s.Activity, s.ActivityAt, s.TurnEndedAt = "%3", model.Idle, at(10), at(10)
			s.LastPromptAt, s.StartedAt, s.LastEventAt = at(2), at(1), at(10)
		})},
		Tmux: TmuxView{OK: true, Panes: []model.TmuxPane{pane("%3", "@7", 2), pane("%9", "@8", 3)},
			Clients: []model.TmuxClient{reporting("c", "%9", true), reporting("d", "%3", focused)}},
	}
}

func TestYourTurnUntilSeen(t *testing.T) {
	snap, mem := compute(yourTurnObs(false), nil, at(11))
	s := find(t, snap, "claude:s1")
	if s.Group != GroupYourTurn || !s.Unseen || !s.CompletedAt.Equal(at(10)) {
		t.Fatalf("want your turn: %+v", s)
	}
	// Started after watchStart: first-sight seen mark is the start time.
	if !mem.Seen["claude:s1"].Equal(at(1)) {
		t.Fatalf("seen = %v", mem.Seen["claude:s1"])
	}
	// A client that attached before focus reporting shows the pane and
	// claims focus (tmux's initial flag, never cleared): not looking, so the
	// turn is still unseen.
	stale := yourTurnObs(false)
	stale.Tmux.Clients = append(stale.Tmux.Clients, model.TmuxClient{Name: "old", Pane: "%3", Focused: true,
		Created: t0.Add(-time.Hour), FocusSince: t0})
	if s := find(t, func() Snapshot { s, _ := compute(stale, &mem, at(15)); return s }(), "claude:s1"); s.Visible || !s.Unseen {
		t.Fatalf("a stale focus flag made the pane visible: %+v", s)
	}
	// Looking at the pane on a focused client acknowledges it.

	snap, mem = compute(yourTurnObs(true), &mem, at(20))
	s = find(t, snap, "claude:s1")
	if s.Unseen || s.Group == GroupYourTurn || !s.Visible {
		t.Fatalf("visible session still flagged: %+v", s)
	}
	if !mem.Seen["claude:s1"].Equal(at(20)) {
		t.Fatalf("seen = %v", mem.Seen["claude:s1"])
	}
	// Once seen it stays seen when the pane is no longer visible.
	snap, _ = compute(yourTurnObs(false), &mem, at(30))
	if s := find(t, snap, "claude:s1"); s.Unseen {
		t.Fatal("seen completion flagged again")
	}
}

func TestComputeDoesNotModifyMemory(t *testing.T) {
	mem := NewMemory()
	mem.Initialized = true
	before := mem.Clone()
	_, out := Compute(yourTurnObs(true), mem, cfg, t0, at(20))
	if !mem.Persisted(before) || len(mem.Seen) != 0 {
		t.Fatalf("input memory modified: %+v", mem)
	}
	if out.Persisted(before) {
		t.Fatal("expected the returned memory to differ")
	}
}

func TestComputeIsDeterministic(t *testing.T) {
	obs := unmanagedAgyObs()
	a, _ := compute(obs, nil, at(100))
	for i := 0; i < 20; i++ {
		b, _ := compute(obs, nil, at(100))
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("run %d differs:\n%v\n%v", i, keys(a), keys(b))
		}
	}
}

func TestFirstRunTreatsExistingCompletionsAsSeen(t *testing.T) {
	obs := yourTurnObs(false)
	snap, mem := Compute(obs, NewMemory(), cfg, at(50), at(60)) // first run ever
	if s := find(t, snap, "claude:s1"); s.Unseen {
		t.Fatal("a completion from before cccp first looked must not be flagged")
	}
	if !mem.Initialized {
		t.Fatal("first computation with sessions should initialise memory")
	}
	// No sessions: stays uninitialised.
	_, mem = Compute(Observation{}, NewMemory(), cfg, t0, at(60))
	if mem.Initialized {
		t.Fatal("initialised without any session")
	}
}

func TestSessionPredatingWatchIsSeenAtFirstSight(t *testing.T) {
	// Not a first run, but the session started before this process began
	// watching (e.g. it ran while the daemon was down).
	snap, mem := Compute(yourTurnObs(false), func() Memory { m := NewMemory(); m.Initialized = true; return m }(), cfg, at(50), at(60))
	if s := find(t, snap, "claude:s1"); s.Unseen {
		t.Fatal("flagged a completion from before watching began")
	}
	if !mem.Seen["claude:s1"].Equal(at(60)) {
		t.Fatalf("seen = %v", mem.Seen["claude:s1"])
	}
}

func TestEndedRetention(t *testing.T) {
	mk := func(endSec int) Observation {
		return Observation{Hooks: []HookRecord{hookRec("s1", 42, false, func(s *model.HookState) {
			s.Activity, s.EndedAt, s.LastEventAt = model.Ended, at(endSec), at(endSec)
		})}}
	}
	snap, _ := compute(mk(0), nil, at(60))
	if s := find(t, snap, "claude:s1"); s.Group != GroupClosed || s.Alive {
		t.Fatalf("%+v", s)
	}
	snap, _ = compute(mk(0), nil, t0.Add(cfg.EndedRetention.Duration+time.Second))
	absent(t, snap, "claude:s1")
}

func TestDeadProcessEndsSession(t *testing.T) {
	obs := Observation{Hooks: []HookRecord{hookRec("s1", 42, false, func(s *model.HookState) {
		s.Activity, s.ActivityAt, s.LastEventAt = model.Working, at(1), at(1)
	})}}
	snap, _ := compute(obs, nil, at(5))
	s := find(t, snap, "claude:s1")
	if s.Alive || s.Group != GroupClosed || !s.EndedAt.Equal(at(1)) {
		t.Fatalf("%+v", s)
	}
}

func TestHookWithoutPIDUsesPaneAgent(t *testing.T) {
	obs := Observation{
		Hooks: []HookRecord{hookRec("s1", 0, false, func(s *model.HookState) {
			s.Pane, s.Activity, s.ActivityAt = "%3", model.Working, at(1)
		})},
		Tmux:   TmuxView{OK: true, Panes: []model.TmuxPane{pane("%3", "@7", 2)}},
		Agents: []AgentProc{{PID: 99, Agent: model.Claude, Pane: "%3", Cmdline: []string{"claude"}}},
	}
	snap, _ := compute(obs, nil, at(5))
	if s := find(t, snap, "claude:s1"); !s.Alive {
		t.Fatalf("pane runs an agent: should be alive: %+v", s)
	}
	obs.Agents = nil
	snap, _ = compute(obs, nil, at(5))
	if s := find(t, snap, "claude:s1"); s.Alive {
		t.Fatal("no agent in the pane: should be ended")
	}
}

// --- Compute: registry -------------------------------------------------------

func TestRegistrySupersedesHookOnSamePID(t *testing.T) {
	obs := Observation{
		Hooks: []HookRecord{
			hookRec("old", 42, true, func(s *model.HookState) { s.Activity, s.ActivityAt, s.LastEventAt = model.Idle, at(1), at(1) }),
			hookRec("new", 42, true, func(s *model.HookState) { s.Activity, s.ActivityAt, s.LastEventAt = model.Working, at(5), at(5) }),
		},
		Registry: []RegistryRecord{regRec("new", 42, "idle", at(9), nil)},
	}
	snap, _ := compute(obs, nil, at(10))
	if s := find(t, snap, "claude:old"); s.Alive || s.Group != GroupClosed {
		t.Fatalf("superseded: %+v", s)
	}
	s := find(t, snap, "claude:new")
	if !s.Alive || s.Tracking != TrackHooksRegistry || s.Activity != model.Idle {
		t.Fatalf("current: %+v", s)
	}
	// No custom or AI title: the registry's derived name beats the cwd.
	if s.Title != "new-1" || s.TitleSource != "name" {
		t.Fatalf("title %q (%s)", s.Title, s.TitleSource)
	}
}

func TestHooksOnSamePIDLatestWins(t *testing.T) {
	// /clear without SessionEnd and no registry: the later session is current.
	obs := Observation{Hooks: []HookRecord{
		hookRec("a", 42, true, func(s *model.HookState) { s.Activity, s.LastEventAt = model.Idle, at(1) }),
		hookRec("b", 42, true, func(s *model.HookState) { s.Activity, s.LastEventAt = model.Idle, at(5) }),
	}}
	snap, _ := compute(obs, nil, at(10))
	if find(t, snap, "claude:a").Alive || !find(t, snap, "claude:b").Alive {
		t.Fatal("latest hook session on a pid should be the live one")
	}
}

func TestRegistryTransitionsAreCompletions(t *testing.T) {
	obs := func(status string, sec int) Observation {
		return Observation{Registry: []RegistryRecord{regRec("r1", 42, status, at(sec), func(e *model.RegistryEntry) {
			e.StartedAtMs = at(1).UnixMilli()
		})}}
	}
	// First observation: no previous status, no completion.
	snap, mem := compute(obs("idle", 2), nil, at(3))
	if s := find(t, snap, "claude:r1"); s.Unseen || !s.CompletedAt.IsZero() {
		t.Fatalf("first observation: %+v", s)
	}
	snap, mem = compute(obs("busy", 4), &mem, at(5))
	if s := find(t, snap, "claude:r1"); s.Group != GroupWorking {
		t.Fatalf("%+v", s)
	}
	snap, mem = compute(obs("idle", 8), &mem, at(9))
	s := find(t, snap, "claude:r1")
	if !s.Unseen || s.Group != GroupYourTurn || !s.CompletedAt.Equal(time.UnixMilli(at(8).UnixMilli())) {
		t.Fatalf("busy→idle should be a completion: %+v", s)
	}
	// idle → idle is not another completion.
	_, mem2 := compute(obs("idle", 8), &mem, at(12))
	if !mem2.RegCompletions["claude:r1"].Equal(mem.RegCompletions["claude:r1"]) {
		t.Fatal("idle→idle changed the completion")
	}
	// waiting → idle (permission answered, turn ended) counts too.
	_, mem = compute(obs("waiting", 20), &mem, at(21))
	_, mem = compute(obs("idle", 25), &mem, at(26))
	if !mem.RegCompletions["claude:r1"].Equal(time.UnixMilli(at(25).UnixMilli())) {
		t.Fatalf("waiting→idle: %v", mem.RegCompletions["claude:r1"])
	}
}

func TestDeadRegistryEntryIgnored(t *testing.T) {
	r := regRec("r1", 42, "busy", at(1), nil)
	r.Alive = false
	snap, _ := compute(Observation{Registry: []RegistryRecord{r}}, nil, at(2))
	absent(t, snap, "claude:r1")
}

func TestPrintModeExcludedEverywhere(t *testing.T) {
	obs := Observation{
		Hooks: []HookRecord{hookRec("script", 41, true, func(s *model.HookState) {
			s.PrintMode, s.Activity = true, model.Idle
		})},
		// claude -p registers as kind "interactive" with entrypoint "sdk-cli".
		Registry: []RegistryRecord{regRec("sdk", 42, "idle", at(1), func(e *model.RegistryEntry) { e.Entrypoint = "sdk-cli" })},
		Tmux:     TmuxView{OK: true, Panes: []model.TmuxPane{pane("%5", "@5", 5)}},
		Agents:   []AgentProc{{PID: 43, Agent: model.Claude, Pane: "%5", Cmdline: []string{"claude", "-p", "hi"}}},
	}
	snap, _ := compute(obs, nil, at(2))
	if len(snap.Sessions) != 0 {
		t.Fatalf("print-mode runs listed: %v", keys(snap))
	}
}

// --- Compute: unmanaged sessions ---------------------------------------------

func TestUnmanagedClaudeByResumeFlag(t *testing.T) {
	sid := "00000000-0000-4000-8000-000000000001"
	obs := Observation{
		Tmux:   TmuxView{OK: true, Panes: []model.TmuxPane{pane("%3", "@3", 3)}},
		Agents: []AgentProc{{PID: 7, Start: 5, Agent: model.Claude, Pane: "%3", Cwd: "/w", Cmdline: []string{"claude", "--resume", sid}}},
	}
	needs := Plan(obs)
	if len(needs.Claude) != 1 || needs.Claude[0].SessionID != sid || needs.Claude[0].Cwd != "/w" || len(needs.Projects) != 0 {
		t.Fatalf("needs: %+v", needs)
	}
	obs.ClaudeTranscripts = map[string]ClaudeTranscript{sid: {Path: "/t.jsonl", Meta: model.ClaudeMeta{
		AITitle: "Gain staging", LastAssistantAt: at(10), LastStopReason: "end_turn", TTL: model.TTL1h,
	}}}
	snap, _ := compute(obs, nil, at(20))
	s := find(t, snap, "claude:"+sid)
	if s.Tracking != TrackUnmanaged || !s.Inferred || s.Activity != model.Idle || s.Title != "Gain staging" {
		t.Fatalf("%+v", s)
	}
	if !s.Cache.Known || !s.Cache.ExpiresAt.Equal(at(10).Add(time.Hour)) || s.Group != GroupIdle {
		t.Fatalf("cache: %+v", s.Cache)
	}
}

func TestUnmanagedClaudeMatchedByTitle(t *testing.T) {
	obs := Observation{
		Hooks: []HookRecord{hookRec("tracked", 50, true, func(s *model.HookState) { s.Activity = model.Idle })},
		Tmux: TmuxView{OK: true, Panes: []model.TmuxPane{func() model.TmuxPane {
			p := pane("%3", "@3", 3)
			p.Title = "✳ AX96 simulation"
			return p
		}()}},
		Agents: []AgentProc{{PID: 7, Agent: model.Claude, Pane: "%3", Cwd: "/w", Cmdline: []string{"claude"}}},
	}
	if needs := Plan(obs); len(needs.Projects) != 1 || needs.Projects[0] != (ProjectKey{Cwd: "/w"}) {
		t.Fatalf("needs: %+v", needs)
	}
	m := model.ClaudeMeta{AITitle: "AX96 simulation", LastAssistantAt: at(3), LastStopReason: "end_turn"}
	obs.Projects = map[ProjectKey][]ProjectTranscript{{Cwd: "/w"}: {
		{SessionID: "older", ModTime: at(1), Meta: m},
		{SessionID: "newer", ModTime: at(2), Meta: m},
		{SessionID: "tracked", ModTime: at(9), Meta: m}, // tracked elsewhere: never matched
		{SessionID: "other", ModTime: at(9), Meta: model.ClaudeMeta{AITitle: "Something else"}},
	}}
	snap, _ := compute(obs, nil, at(20))
	s := find(t, snap, "claude:newer")
	if s.Title != "AX96 simulation" || s.Activity != model.Idle || !strings.Contains(strings.Join(s.Notes, ";"), "several transcripts") {
		t.Fatalf("%+v", s)
	}
}

func TestUnmanagedClaudeNoTranscript(t *testing.T) {
	p := pane("%3", "@3", 3)
	p.Title = "✳ Long gone"
	p.WindowActivity = at(-3600)
	obs := Observation{
		Tmux:     TmuxView{OK: true, Panes: []model.TmuxPane{p}},
		Agents:   []AgentProc{{PID: 7, Agent: model.Claude, Pane: "%3", Cwd: "/w", Cmdline: []string{"claude"}}},
		Projects: map[ProjectKey][]ProjectTranscript{{Cwd: "/w"}: nil},
	}
	snap, _ := compute(obs, nil, at(0))
	s := find(t, snap, "claude:pane:%3")
	if s.Group != GroupUnknown || s.Title != "Long gone" || s.TitleSource != "pane" || s.SessionID != "" {
		t.Fatalf("%+v", s)
	}
}

func TestUnmanagedInference(t *testing.T) {
	sid := "00000000-0000-4000-8000-000000000001"
	mk := func(meta model.ClaudeMeta, outputAgo time.Duration, panes int) Session {
		p := pane("%3", "@3", 3)
		p.WindowPanes = panes
		p.WindowActivity = at(100).Add(-outputAgo)
		obs := Observation{
			Tmux:              TmuxView{OK: true, Panes: []model.TmuxPane{p}},
			Agents:            []AgentProc{{PID: 7, Agent: model.Claude, Pane: "%3", Cmdline: []string{"claude", "-r", sid}}},
			ClaudeTranscripts: map[string]ClaudeTranscript{sid: {Path: "/t", Meta: meta}},
		}
		snap, _ := compute(obs, nil, at(100))
		return find(t, snap, "claude:"+sid)
	}
	pending := model.ClaudeMeta{PendingTool: true, PendingToolName: "Bash", LastAssistantAt: at(50)}
	if s := mk(pending, time.Minute, 1); s.Activity != model.NeedsApproval || s.Tool != "Bash" || !s.Inferred {
		t.Errorf("pending tool, quiet pane: %+v", s)
	}
	if s := mk(pending, time.Second, 1); s.Activity != model.Working {
		t.Errorf("pending tool, pane producing output: %+v", s)
	}
	if s := mk(pending, time.Minute, 2); s.Activity != model.Working {
		t.Errorf("split window: output age unknown, must not infer a wait: %+v", s)
	}
	thinking := model.ClaudeMeta{LastUserAt: at(90), LastAssistantAt: at(10)}
	if s := mk(thinking, time.Second, 1); s.Activity != model.Working {
		t.Errorf("prompt sent: %+v", s)
	}
	if s := mk(thinking, 2*time.Minute, 1); s.Activity != model.Unknown {
		t.Errorf("prompt sent, long quiet: %+v", s)
	}
	interrupted := model.ClaudeMeta{LastUserAt: at(90), LastAssistantAt: at(10), Interrupted: true}
	if s := mk(interrupted, time.Minute, 1); s.Activity != model.Idle {
		t.Errorf("interrupted: %+v", s)
	}
}

func unmanagedAgyObs() Observation {
	return Observation{
		Tmux: TmuxView{OK: true, Panes: []model.TmuxPane{pane("%1", "@1", 1), pane("%2", "@2", 2)}},
		Agents: []AgentProc{
			{PID: 10, Start: 100, StartedAt: at(0), Agent: model.Agy, Pane: "%1", Cwd: "/w", Cmdline: []string{"agy"}},
			{PID: 11, Start: 200, StartedAt: at(30), Agent: model.Agy, Pane: "%2", Cwd: "/w", Cmdline: []string{"agy"}},
		},
		AgyConversations: map[string]model.AgyConversation{
			"conv-old":   {ID: "conv-old", Title: "Old", Workspaces: []string{"/w"}, LastModified: at(20)},
			"conv-new":   {ID: "conv-new", Title: "New", Workspaces: []string{"/w"}, LastModified: at(40)},
			"conv-other": {ID: "conv-other", Workspaces: []string{"/elsewhere"}, LastModified: at(50)},
			"conv-stale": {ID: "conv-stale", Workspaces: []string{"/w"}, LastModified: at(-10)},
		},
	}
}

func TestUnmanagedAgyProcessesGetDistinctConversations(t *testing.T) {
	obs := unmanagedAgyObs()
	needs := Plan(obs)
	if len(needs.Agy) != 2 {
		t.Fatalf("needs: %+v", needs)
	}
	snap, _ := compute(obs, nil, at(60))
	byPane := map[string]Session{}
	for _, s := range snap.Sessions {
		byPane[s.Pane] = s
	}
	// The newest process owns the newest conversation; the older process
	// gets the newest conversation modified since it started.
	if byPane["%2"].SessionID != "conv-new" || byPane["%1"].SessionID != "conv-old" {
		t.Fatalf("%%1=%s %%2=%s", byPane["%1"].SessionID, byPane["%2"].SessionID)
	}
	if byPane["%2"].Title != "New" {
		t.Fatalf("title %q", byPane["%2"].Title)
	}
}

func TestHostnamePaneTitleIgnored(t *testing.T) {
	p := pane("%1", "@1", 1)
	p.Title = "laptop"
	obs := Observation{
		Hostname: "laptop",
		Tmux:     TmuxView{OK: true, Panes: []model.TmuxPane{p}},
		Agents:   []AgentProc{{PID: 10, Agent: model.Agy, Pane: "%1", Cwd: "/w/logger", Cmdline: []string{"agy"}}},
	}
	snap, _ := compute(obs, nil, at(1))
	if s := snap.Sessions[0]; s.Title != "logger" || s.TitleSource != "cwd" {
		t.Fatalf("title %q (%s)", s.Title, s.TitleSource)
	}
}

func TestPaneAgentsPickShallowestNonPrintMode(t *testing.T) {
	got := paneAgents([]AgentProc{
		{PID: 1, Pane: "%1", Depth: 1, Cmdline: []string{"claude", "-p", "x"}},
		{PID: 2, Pane: "%1", Depth: 2, Cmdline: []string{"claude"}},
		{PID: 3, Pane: "%1", Depth: 3, Cmdline: []string{"claude"}},
	})
	if len(got) != 1 || got[2].PID != 2 {
		t.Fatalf("%+v", got)
	}
}

// --- cache, ordering, deadlines, navigation, pruning ---------------------------

func cacheObs(sid string, last time.Time, ttl time.Duration) Observation {
	return Observation{
		Hooks: []HookRecord{hookRec(sid, 42, true, func(s *model.HookState) { s.Activity, s.ActivityAt = model.Idle, last })},
		ClaudeTranscripts: map[string]ClaudeTranscript{sid: {Path: "/t", Meta: model.ClaudeMeta{
			LastAssistantAt: last, TTL: ttl,
		}}},
	}
}

func TestCacheWarmColdAndAssumedTTL(t *testing.T) {
	snap, _ := compute(cacheObs("s1", at(0), model.TTL5m), nil, at(60))
	if s := find(t, snap, "claude:s1"); s.Group != GroupIdle || s.Cache.TTLAssumed {
		t.Fatalf("%+v", s)
	}
	snap, _ = compute(cacheObs("s1", at(0), model.TTL5m), nil, at(301))
	if s := find(t, snap, "claude:s1"); s.Group != GroupCold {
		t.Fatalf("%+v", s)
	}
	snap, _ = compute(cacheObs("s1", at(0), 0), nil, at(60))
	if s := find(t, snap, "claude:s1"); !s.Cache.TTLAssumed || s.Cache.TTL != cfg.DefaultCacheTTL.Duration {
		t.Fatalf("%+v", s.Cache)
	}
}

func TestOrderingAndDeadline(t *testing.T) {
	obs := cacheObs("a", at(0), model.TTL1h)
	b := cacheObs("b", at(100), model.TTL5m)
	obs.Hooks = append(obs.Hooks, b.Hooks...)
	obs.Hooks[1].State.PID = 43
	obs.ClaudeTranscripts["b"] = b.ClaudeTranscripts["b"]
	wait := hookRec("w", 44, true, func(s *model.HookState) { s.Activity, s.ActivityAt = model.NeedsApproval, at(50) })
	obs.Hooks = append(obs.Hooks, wait)
	snap, _ := compute(obs, nil, at(200))
	if got := keys(snap); !reflect.DeepEqual(got, []string{"claude:w", "claude:b", "claude:a"}) {
		t.Fatalf("order %v (needs you first; idle by soonest expiry)", got)
	}
	if d := NextDeadline(snap); !d.Equal(at(100).Add(5 * time.Minute)) {
		t.Fatalf("deadline %v", d)
	}
}

func TestNextAttention(t *testing.T) {
	snap := Snapshot{Sessions: []Session{
		{Key: "1", Group: GroupNeedsYou, Alive: true, Pane: "%1"},
		{Key: "2", Group: GroupYourTurn, Alive: true, Pane: "%2"},
		{Key: "3", Group: GroupWorking, Alive: true, Pane: "%3"},
		{Key: "4", Group: GroupYourTurn, Alive: true, Kind: "background"},
		{Key: "5", Group: GroupYourTurn, Alive: true}, // no pane, not background: unreachable
	}}
	for cur, want := range map[string]string{"": "1", "%9": "1", "%1": "2", "%2": "4", "%3": "1"} {
		got, ok := NextAttention(snap, cur)
		if !ok || got.Key != want {
			t.Errorf("from %q: got %q want %q", cur, got.Key, want)
		}
	}
	if _, ok := NextAttention(Snapshot{Sessions: []Session{{Group: GroupWorking, Alive: true, Pane: "%1"}}}, ""); ok {
		t.Fatal("nothing needs attention")
	}
}

func TestExpired(t *testing.T) {
	ret := cfg.EndedRetention.Duration
	now := t0.Add(ret + time.Hour)
	obs := Observation{Hooks: []HookRecord{
		hookRec("live", 1, true, func(s *model.HookState) { s.Activity, s.LastEventAt = model.Idle, t0 }),
		hookRec("ended-old", 2, false, func(s *model.HookState) { s.Activity, s.EndedAt = model.Ended, t0 }),
		hookRec("ended-recent", 3, false, func(s *model.HookState) { s.Activity, s.EndedAt = model.Ended, now.Add(-time.Minute) }),
		hookRec("dead-old", 4, false, func(s *model.HookState) { s.Activity, s.LastEventAt = model.Working, t0 }),
		hookRec("print-just-ended", 5, false, func(s *model.HookState) { s.PrintMode, s.Activity, s.EndedAt = true, model.Ended, now }),
		hookRec("print-running", 6, true, func(s *model.HookState) { s.PrintMode, s.Activity = true, model.Working }),
	}}
	got := Expired(obs, cfg, now)
	want := []string{"/state/ended-old.json", "/state/dead-old.json", "/state/print-just-ended.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestMemoryPruneKeepsAliases(t *testing.T) {
	m := NewMemory()
	m.Seen["a"], m.Seen["b"] = t0, t0
	m.Aliases["b"] = "build box"
	m.Summaries["b"] = SummaryRecord{Text: "x"}
	p := m.Prune(map[string]bool{"a": true})
	if _, ok := p.Seen["b"]; ok {
		t.Fatal("seen not pruned")
	}
	if _, ok := p.Summaries["b"]; ok {
		t.Fatal("summary not pruned")
	}
	if p.Aliases["b"] != "build box" {
		t.Fatal("aliases must survive pruning")
	}
	if len(m.Seen) != 2 {
		t.Fatal("Prune modified its receiver")
	}
}

func TestPlanJump(t *testing.T) {
	cases := []struct {
		s    Session
		want Jump
	}{
		{Session{Alive: true, Pane: "%3"}, Jump{Kind: JumpPane, Pane: "%3"}},
		{Session{Alive: true, Kind: "background", SessionID: "b1", Cwd: "/w"}, Jump{Kind: JumpNewWindow, Dir: "/w", Argv: []string{"env", "-u", "CLAUDE_CONFIG_DIR", "claude", "attach", "b1"}}},
		{Session{Alive: false, Agent: model.Claude, SessionID: "c1", Cwd: "/w"}, Jump{Kind: JumpNewWindow, Dir: "/w", Argv: []string{"env", "-u", "CLAUDE_CONFIG_DIR", "claude", "--resume", "c1"}, Resume: true}},
		{Session{Alive: false, Agent: model.Agy, SessionID: "a1", Cwd: "/w"}, Jump{Kind: JumpNewWindow, Dir: "/w", Argv: []string{"agy", "--conversation=a1"}, Resume: true}},
	}
	for i, c := range cases {
		if got := PlanJump(c.s, nil); !reflect.DeepEqual(got, c.want) {
			t.Errorf("case %d: %+v want %+v", i, got, c.want)
		}
	}
	for _, s := range []Session{
		{Alive: true, Title: "x"},                       // live, no pane, not background
		{Alive: false, Agent: model.Claude, Title: "y"}, // ended, id unknown
	} {
		if j := PlanJump(s, nil); j.Kind != JumpImpossible || j.Reason == "" {
			t.Errorf("%+v: %+v", s, j)
		}
	}
}

// --- rules pinned by mutation testing ------------------------------------------

func TestCompletionBeforeLatestPromptIsNotFlagged(t *testing.T) {
	// Turn 1 completed (unseen), then the user prompted again and
	// interrupted: idle, but the old completion predates the prompt.
	obs := Observation{Hooks: []HookRecord{hookRec("s1", 42, true, func(s *model.HookState) {
		s.Activity, s.ActivityAt, s.TurnEndedAt = model.Idle, at(30), at(10)
		s.LastPromptAt, s.StartedAt = at(20), at(1)
	})}}
	snap, _ := compute(obs, nil, at(40))
	if s := find(t, snap, "claude:s1"); s.Unseen {
		t.Fatalf("stale completion flagged: %+v", s)
	}
}

func TestTranscriptEndTurnIsACompletionOnlyForUnmanaged(t *testing.T) {
	// A registry session whose transcript ended a turn, with no busy→idle
	// transition observed: observed signals decide, so no completion.
	obs := Observation{
		Registry: []RegistryRecord{regRec("r1", 42, "idle", at(5), func(e *model.RegistryEntry) { e.StartedAtMs = at(1).UnixMilli() })},
		ClaudeTranscripts: map[string]ClaudeTranscript{"r1": {Path: "/t", Meta: model.ClaudeMeta{
			LastAssistantAt: at(5), LastStopReason: "end_turn"}}},
	}
	snap, _ := compute(obs, nil, at(6))
	if s := find(t, snap, "claude:r1"); s.Unseen || !s.CompletedAt.IsZero() {
		t.Fatalf("%+v", s)
	}
}

func TestRegistryOwnsItsProcessEvenAgainstNewerHookEvents(t *testing.T) {
	obs := Observation{
		Hooks: []HookRecord{hookRec("old", 42, true, func(s *model.HookState) {
			s.Activity, s.ActivityAt, s.LastEventAt = model.Working, at(20), at(20) // newer than the registry
		})},
		Registry: []RegistryRecord{regRec("new", 42, "idle", at(9), nil)},
	}
	snap, _ := compute(obs, nil, at(30))
	if s := find(t, snap, "claude:old"); s.Alive {
		t.Fatalf("the registry names another session for pid 42: %+v", s)
	}
	if s := find(t, snap, "claude:new"); !s.Alive {
		t.Fatal("registry session not current")
	}
}

func TestResumeFlagMustBeASessionID(t *testing.T) {
	p := pane("%3", "@3", 3)
	p.Title = "✳ Named session"
	obs := Observation{
		Tmux:     TmuxView{OK: true, Panes: []model.TmuxPane{p}},
		Agents:   []AgentProc{{PID: 7, Agent: model.Claude, Pane: "%3", Cwd: "/w", Cmdline: []string{"claude", "--resume", "my-session-name"}}},
		Projects: map[ProjectKey][]ProjectTranscript{{Cwd: "/w"}: {{SessionID: "real-id", Meta: model.ClaudeMeta{AITitle: "Named session"}}}},
	}
	snap, _ := compute(obs, nil, at(1))
	for _, s := range snap.Sessions {
		if s.SessionID == "my-session-name" {
			t.Fatal("a session name was taken as a session id")
		}
	}
	find(t, snap, "claude:real-id") // resolved by title instead
}

// --- profiles --------------------------------------------------------------------

var profiles = []model.Profile{
	{Name: "personal", Dir: "/h/.claude", Default: true},
	{Name: "work", Dir: "/h/.claude-work"},
}

func TestSessionsAreAttributedToTheirProfile(t *testing.T) {
	reg := regRec("r1", 42, "idle", at(1), nil)
	reg.ConfigDir = "/h/.claude-work"
	obs := Observation{
		Profiles: profiles,
		Registry: []RegistryRecord{reg},
		Hooks: []HookRecord{
			hookRec("h-default", 43, true, func(s *model.HookState) { s.Activity = model.Idle }),
			hookRec("h-work", 44, true, func(s *model.HookState) { s.Activity, s.ConfigDir = model.Idle, "/h/.claude-work" }),
		},
	}
	needs := Plan(obs)
	wantDirs := map[string]string{"h-default": "/h/.claude", "h-work": "/h/.claude-work", "r1": "/h/.claude-work"}
	for _, n := range needs.Claude {
		if n.ConfigDir != wantDirs[n.SessionID] {
			t.Errorf("need %s: config dir %q want %q", n.SessionID, n.ConfigDir, wantDirs[n.SessionID])
		}
	}
	snap, _ := compute(obs, nil, at(2))
	for sid, want := range map[string]string{"h-default": "personal", "h-work": "work", "r1": "work"} {
		s := find(t, snap, "claude:"+sid)
		if s.Profile != want || s.ConfigDir != wantDirs[sid] || s.DefaultProfile != (sid == "h-default") {
			t.Errorf("%s: profile %q dir %q default %v", sid, s.Profile, s.ConfigDir, s.DefaultProfile)
		}
	}
}

func TestUnmanagedSessionMatchedWithinItsProfile(t *testing.T) {
	p := pane("%3", "@3", 3)
	p.Title = "✳ Work thing"
	m := model.ClaudeMeta{AITitle: "Work thing", LastAssistantAt: at(1), LastStopReason: "end_turn"}
	obs := Observation{
		Profiles: profiles,
		Tmux:     TmuxView{OK: true, Panes: []model.TmuxPane{p}},
		Agents:   []AgentProc{{PID: 7, Agent: model.Claude, Pane: "%3", Cwd: "/w", ConfigDir: "/h/.claude-work", Cmdline: []string{"claude"}}},
	}
	needs := Plan(obs)
	if len(needs.Projects) != 1 || needs.Projects[0] != (ProjectKey{ConfigDir: "/h/.claude-work", Cwd: "/w"}) {
		t.Fatalf("needs %+v", needs.Projects)
	}
	// The same title in the other profile's project must not match.
	obs.Projects = map[ProjectKey][]ProjectTranscript{
		{ConfigDir: "/h/.claude", Cwd: "/w"}:      {{SessionID: "personal-one", ModTime: at(9), Meta: m}},
		{ConfigDir: "/h/.claude-work", Cwd: "/w"}: {{SessionID: "work-one", ModTime: at(1), Meta: m}},
	}
	snap, _ := compute(obs, nil, at(5))
	s := find(t, snap, "claude:work-one")
	if s.Profile != "work" || s.Title != "Work thing" {
		t.Fatalf("%+v", s)
	}
	absent(t, snap, "claude:personal-one")
}

func TestPlanJumpSetsTheProfile(t *testing.T) {
	ended := Session{Agent: model.Claude, SessionID: "c1", Cwd: "/w", ConfigDir: "/h/.claude-work"}
	if j := PlanJump(ended, nil); !reflect.DeepEqual(j.Env, []string{"CLAUDE_CONFIG_DIR=/h/.claude-work"}) {
		t.Fatalf("resume env %v", j.Env)
	}
	if j := PlanJump(ended, nil); !reflect.DeepEqual(j.Argv, []string{"claude", "--resume", "c1"}) {
		t.Fatalf("resume argv %v", j.Argv)
	}
	// The default profile is selected by removing the variable, in the
	// command itself: CLAUDE_CONFIG_DIR=~/.claude would start Claude Code
	// from a blank ~/.claude/.claude.json (the first-start flow).
	bg := Session{Alive: true, Kind: "background", Agent: model.Claude, SessionID: "b1", ConfigDir: "/h/.claude", DefaultProfile: true}
	if j := PlanJump(bg, nil); j.Env != nil || !reflect.DeepEqual(j.Argv, []string{"env", "-u", "CLAUDE_CONFIG_DIR", "claude", "attach", "b1"}) {
		t.Fatalf("attach under the default profile: argv %v env %v", j.Argv, j.Env)
	}
	if j := PlanJump(Session{Agent: model.Agy, SessionID: "a1", Cwd: "/w"}, nil); j.Env != nil {
		t.Fatalf("agy env %v", j.Env)
	}
}

// --- closed conversations (the catalog) ------------------------------------------

func conv(agent model.Agent, sid string, lastSec int) Conversation {
	return Conversation{Agent: agent, SessionID: sid, ConfigDir: "/h/.claude", Path: "/t/" + sid + ".jsonl",
		Cwd: "/w/proj", Title: "Title " + sid, FirstPrompt: "first prompt of " + sid, LastPrompt: "last prompt",
		LastMessage: "last message", Branch: "main", Model: "claude-x", Turns: 7, First: at(0), Last: at(lastSec)}
}

func TestCatalogOnlyConversationIsClosed(t *testing.T) {
	obs := Observation{Profiles: profiles, Conversations: []Conversation{conv(model.Claude, "c1", 50)}}
	if needs := Plan(obs); len(needs.Claude) != 0 {
		t.Fatalf("a closed conversation must not ask for its transcript: %+v", needs.Claude)
	}
	// Long after the retention period: still listed while the transcript exists.
	now := t0.Add(cfg.EndedRetention.Duration * 3)
	snap, _ := compute(obs, nil, now)
	s := find(t, snap, "claude:c1")
	if s.Group != GroupClosed || s.Alive || s.Tracking != TrackCatalog || s.Activity != model.Ended {
		t.Fatalf("%+v", s)
	}
	if s.Title != "Title c1" || s.FirstPrompt != "first prompt of c1" || s.LastMessage != "last message" ||
		s.Turns != 7 || s.Branch != "main" || s.Profile != "personal" || s.TranscriptPath != "/t/c1.jsonl" ||
		!s.ActivitySince.Equal(at(50)) || !s.StartedAt.Equal(at(0)) {
		t.Fatalf("%+v", s)
	}
}

func TestClosedConversationsNewestFirst(t *testing.T) {
	obs := Observation{Conversations: []Conversation{conv(model.Claude, "old", 10), conv(model.Claude, "new", 90), conv(model.Agy, "mid", 50)}}
	snap, _ := compute(obs, nil, at(100))
	if got := keys(snap); !reflect.DeepEqual(got, []string{"claude:new", "agy:mid", "claude:old"}) {
		t.Fatalf("%v", got)
	}
}

func TestLiveSessionAndItsConversationAreOneRow(t *testing.T) {
	obs := Observation{
		Profiles:      profiles,
		Registry:      []RegistryRecord{regRec("c1", 42, "busy", at(5), nil)},
		Conversations: []Conversation{conv(model.Claude, "c1", 4)},
	}
	snap, _ := compute(obs, nil, at(6))
	if len(snap.Sessions) != 1 {
		t.Fatalf("one conversation, one row: %v", keys(snap))
	}
	s := snap.Sessions[0]
	if s.Group != GroupWorking || s.Turns != 7 || s.FirstPrompt != "first prompt of c1" {
		t.Fatalf("%+v", s)
	}
}

func TestEndedHookSessionWithTranscriptOutlivesRetention(t *testing.T) {
	obs := Observation{
		Hooks: []HookRecord{hookRec("c1", 42, false, func(s *model.HookState) {
			s.Activity, s.EndedAt, s.LastEventAt = model.Ended, at(0), at(0)
		})},
		Conversations: []Conversation{conv(model.Claude, "c1", 0)},
	}
	snap, _ := compute(obs, nil, t0.Add(cfg.EndedRetention.Duration*2))
	if s := find(t, snap, "claude:c1"); s.Group != GroupClosed {
		t.Fatalf("%+v", s)
	}
}

func TestSessionForDir(t *testing.T) {
	panes := []model.TmuxPane{
		{Session: "alpha", Path: "/w/acme"}, {Session: "alpha", Path: "/w/acme/tools"},
		{Session: "beta", Path: "/w/acme"},
		{Session: "gamma", Path: "/w/songs"}, {Session: "omega", Path: "/w/songs"},
		{Session: "x", Path: "/w/acme-other"}, // a sibling, not below
	}
	for dir, want := range map[string]string{
		"/w/acme": "alpha", "/w/songs": "gamma", "/w/none": "", "": "",
		// A Claude agent worktree belongs with its project...
		"/w/songs/.claude/worktrees/agent-1": "gamma",
		// ...whichever directory the project is.
		"/w/acme/tools/.claude/worktrees/x": "alpha",
		// Not a worktree: no walking up to a parent.
		"/w/acme-other/sub":         "",
		"/w/songs/.claude/skills/x": "",
	} {
		if got := SessionForDir(panes, dir); got != want {
			t.Errorf("%q → %q want %q", dir, got, want)
		}
	}
}

func TestPlanJumpForClosedConversations(t *testing.T) {
	panes := []model.TmuxPane{{Session: "alpha", Path: "/w/proj"}}
	j := PlanJump(Session{Agent: model.Claude, SessionID: "c1", Cwd: "/w/proj", ConfigDir: "/h/.claude-work"}, panes)
	want := Jump{Kind: JumpNewWindow, Dir: "/w/proj", Argv: []string{"claude", "--resume", "c1"},
		Env: []string{"CLAUDE_CONFIG_DIR=/h/.claude-work"}, TmuxSession: "alpha", Resume: true}
	if !reflect.DeepEqual(j, want) {
		t.Fatalf("%+v", j)
	}
	if j := PlanJump(Session{Agent: model.Claude, SessionID: "c1"}, panes); j.Kind != JumpImpossible || !strings.Contains(j.Reason, "working directory") {
		t.Fatalf("missing cwd: %+v", j)
	}
	j = PlanJump(Session{Alive: true, Agent: model.Claude, SessionID: "c1", PID: 77, Title: "T"}, panes)
	if j.Kind != JumpImpossible || !strings.Contains(j.Reason, "outside tmux (pid 77)") {
		t.Fatalf("open outside tmux: %+v", j)
	}
}

// --- tmux servers ----------------------------------------------------------------

func TestPanesOnAnotherTmuxServerAreNotOurs(t *testing.T) {
	const ours, other = "/tmp/tmux-1000/default", "/tmp/tmux-1000/work"
	obs := func(regSock, hookSock string) Observation {
		return Observation{
			Tmux: TmuxView{OK: true, Socket: ours, Panes: []model.TmuxPane{pane("%2", "@2", 1)}},
			Registry: []RegistryRecord{func() RegistryRecord {
				r := regRec("r", 50, "idle", t0, func(e *model.RegistryEntry) { e.Tmux = "work:@2.%2" })
				r.TmuxSocket = regSock
				return r
			}()},
			Hooks: []HookRecord{hookRec("h", 51, true, func(s *model.HookState) {
				s.Activity, s.Pane, s.TmuxSocket = model.Idle, "%2", hookSock
			})},
		}
	}
	// Both claim %2, recorded on another server: %2 here is someone else's.
	snap, _ := compute(obs(other, other), nil, at(1))
	for _, k := range []string{"claude:r", "claude:h"} {
		if s := find(t, snap, k); s.Pane != "" || s.Location != "" || !s.Alive {
			t.Errorf("%s: pane %q location %q alive %v; want alive, in no pane of ours", k, s.Pane, s.Location, s.Alive)
		}
	}
	// Recorded on this server: theirs.
	snap, _ = compute(obs(ours, ours), nil, at(1))
	for _, k := range []string{"claude:r", "claude:h"} {
		if s := find(t, snap, k); s.Pane != "%2" {
			t.Errorf("%s: pane %q on our server", k, s.Pane)
		}
	}
	// Recorded before servers were (or ours unknown): trusted, as before.
	snap, _ = compute(obs("", ""), nil, at(1))
	if s := find(t, snap, "claude:h"); s.Pane != "%2" {
		t.Errorf("legacy hook state: pane %q", s.Pane)
	}
}

func TestAHookWithoutAPidIsNotAliveInAnotherServersPane(t *testing.T) {
	o := Observation{
		Tmux:   TmuxView{OK: true, Socket: "/tmp/tmux-1000/default", Panes: []model.TmuxPane{pane("%2", "@2", 1)}},
		Agents: []AgentProc{{PID: 77, Agent: model.Claude, Pane: "%2"}}, // an unrelated agent in our %2
		Hooks: []HookRecord{hookRec("h", 0, false, func(s *model.HookState) {
			s.Activity, s.Pane, s.TmuxSocket = model.Idle, "%2", "/tmp/tmux-1000/work"
		})},
	}
	snap, _ := compute(o, nil, at(1))
	for _, s := range snap.Sessions {
		if s.Key == "claude:h" && s.Alive {
			t.Fatal("liveness taken from an agent in a same-numbered pane of another server")
		}
	}
}

// --- workstreams --------------------------------------------------------------------

func TestWorkstreamsAreRecordedWithWhatRunsInEachPane(t *testing.T) {
	wsPane := func(id string, idx int, path string) model.TmuxPane {
		p := pane(id, "@1", 1)
		p.SessionID, p.PaneIndex, p.Path, p.WindowPanes = "$1", idx, path, 3
		p.ServerPID, p.ServerStart, p.WindowLayout, p.WindowAutoName = 10, 100, "b25d,80x24,0,0,0", true
		return p
	}
	placeholder := wsPane("%2", 1, "/w/old")
	placeholder.Resume = workstream.EncodeTag(workstream.Conversation{Agent: model.Claude, SessionID: "old1", Title: "Earlier work"})
	obs := Observation{
		Tmux: TmuxView{OK: true, Socket: "/tmp/tmux-1000/default", ServerPID: 10, ServerStart: 100,
			Panes: []model.TmuxPane{wsPane("%1", 0, "/w/live"), placeholder, wsPane("%3", 2, "/w/shell")}},
		Profiles: []model.Profile{{Name: "work", Dir: "/h/.claude-work"}, {Name: "default", Dir: "/h/.claude", Default: true}},
		Hooks: []HookRecord{hookRec("live1", 42, true, func(s *model.HookState) {
			s.Activity, s.Pane, s.ConfigDir, s.Cwd = model.Idle, "%1", "/h/.claude-work", "/w/live"
		})},
	}
	snap, mem := compute(obs, nil, at(1))
	w, ok := snap.Workstreams.Find("s")
	if !ok || len(w.Windows) != 1 || len(w.Windows[0].Panes) != 3 {
		t.Fatalf("%+v", snap.Workstreams)
	}
	p := w.Windows[0].Panes
	if p[0].Kind != workstream.KindConversation || p[0].Conv.SessionID != "live1" || p[0].Conv.ConfigDir != "/h/.claude-work" || p[0].Conv.DefaultProfile {
		t.Fatalf("live conversation %+v", p[0])
	}
	if p[1].Kind != workstream.KindConversation || p[1].Conv.SessionID != "old1" || p[1].Conv.Title != "Earlier work" {
		t.Fatalf("a placeholder keeps its conversation: %+v", p[1])
	}
	if p[2].Kind != workstream.KindShell || p[2].Cwd != "/w/shell" {
		t.Fatalf("shell %+v", p[2])
	}
	if !mem.Workstreams.Equal(snap.Workstreams) {
		t.Fatal("memory and snapshot disagree")
	}
	// The session closes; the server runs on (no panes left): pending, and
	// the snapshot's deadline is when it is forgotten.
	gone := Observation{Tmux: TmuxView{OK: true, Socket: "/tmp/tmux-1000/default", ServerPID: 10, ServerStart: 100}}
	snap, mem = compute(gone, &mem, at(2))
	if len(snap.Workstreams.Pending) != 1 || !NextDeadline(snap).Equal(at(2).Add(workstream.Grace)) {
		t.Fatalf("pending %+v deadline %v", snap.Workstreams.Pending, NextDeadline(snap))
	}
	snap, _ = compute(gone, &mem, at(2).Add(workstream.Grace))
	if len(snap.Workstreams.Workstreams) != 0 {
		t.Fatalf("not forgotten: %+v", snap.Workstreams)
	}
}

func TestWorkstreamsGoDormantWithoutAServer(t *testing.T) {
	obs := Observation{Tmux: TmuxView{OK: true, ServerPID: 10, ServerStart: 100, Panes: []model.TmuxPane{
		{ID: "%1", Session: "s", SessionID: "$1", WindowID: "@1", Path: "/w"}}}}
	_, mem := compute(obs, nil, at(1))
	snap, mem := compute(Observation{Tmux: TmuxView{NoServer: true, Err: "no server"}}, &mem, at(2))
	if d := snap.Workstreams.Dormant(); len(d) != 1 || d[0].Name != "s" {
		t.Fatalf("%+v", snap.Workstreams)
	}
	// A tmux failure (not "no server") concludes nothing.
	snap, _ = compute(Observation{Tmux: TmuxView{Err: "timeout"}}, &mem, at(3))
	if !snap.Workstreams.Equal(mem.Workstreams) {
		t.Fatal("changed on a tmux failure")
	}
}

// A placeholder that has been resumed keeps its mark while the conversation
// runs; the live session is what is recorded.
func TestALiveConversationOutranksAPlaceholderMark(t *testing.T) {
	p := pane("%2", "@1", 1)
	p.SessionID, p.ServerPID, p.ServerStart = "$1", 10, 100
	p.Resume = workstream.EncodeTag(workstream.Conversation{Agent: model.Claude, SessionID: "old1", Title: "Old title"})
	obs := Observation{
		Tmux:     TmuxView{OK: true, ServerPID: 10, ServerStart: 100, Panes: []model.TmuxPane{p}},
		Profiles: []model.Profile{{Name: "default", Dir: "/h/.claude", Default: true}},
		Hooks: []HookRecord{hookRec("live2", 42, true, func(s *model.HookState) {
			s.Activity, s.Pane = model.Idle, "%2"
		})},
	}
	snap, _ := compute(obs, nil, at(1))
	w, _ := snap.Workstreams.Find("s")
	if c := w.Windows[0].Panes[0].Conv; c.SessionID != "live2" || !c.DefaultProfile {
		t.Fatalf("%+v", c)
	}
}

func TestPlanMove(t *testing.T) {
	inPane := Session{Alive: true, Pane: "%3", WindowID: "@7", TmuxSess: "infra", Title: "T"}
	if m := PlanMove(inPane, "docs"); m.Kind != MoveWindow || m.Window != "@7" {
		t.Fatalf("%+v", m)
	}
	if m := PlanMove(inPane, "infra"); m.Kind != MoveImpossible || !strings.Contains(m.Reason, "already in infra") {
		t.Fatalf("%+v", m)
	}
	closed := Session{Agent: model.Claude, SessionID: "c1", Cwd: "/w", ConfigDir: "/h/.claude-work"}
	m := PlanMove(closed, "docs")
	if m.Kind != MoveOpen || m.Open.TmuxSession != "docs" || !m.Open.Resume || m.Open.Dir != "/w" ||
		!reflect.DeepEqual(m.Open.Argv, []string{"claude", "--resume", "c1"}) {
		t.Fatalf("closed: %+v", m)
	}
	bg := Session{Alive: true, Kind: "background", Agent: model.Claude, SessionID: "b1", Cwd: "/w", DefaultProfile: true}
	if m := PlanMove(bg, "docs"); m.Kind != MoveOpen || m.Open.Argv[len(m.Open.Argv)-2] != "attach" || m.Open.TmuxSession != "docs" {
		t.Fatalf("background: %+v", m)
	}
	outside := Session{Alive: true, Agent: model.Claude, SessionID: "o1", PID: 9, Title: "Out"}
	if m := PlanMove(outside, "docs"); m.Kind != MoveImpossible || !strings.Contains(m.Reason, "outside tmux") {
		t.Fatalf("outside tmux: %+v", m)
	}
}

// agy's cache is estimated from its last model call: likely warm, then
// cooling, then cold (Gemini's implicit cache has no TTL).
func TestAgyCacheIsEstimated(t *testing.T) {
	obs := func() Observation {
		return Observation{
			Tmux:   TmuxView{OK: true, Panes: []model.TmuxPane{pane("%4", "@4", 4)}},
			Agents: []AgentProc{{PID: 50, Agent: model.Agy, Pane: "%4", Cwd: "/w"}},
			Hooks: []HookRecord{hookRec("a1", 50, true, func(s *model.HookState) {
				s.Agent, s.Activity, s.Pane, s.TranscriptPath = model.Agy, model.Idle, "%4", "/t/a1"
				s.ActivityAt, s.LastEventAt = at(0), at(0)
			})},
			AgyTranscripts: map[string]AgyTranscript{"a1": {Path: "/t/a1", Meta: model.AgyMeta{LastModelCallAt: at(0), LastStepAt: at(0)}}},
		}
	}
	for _, c := range []struct {
		after time.Duration
		group Group
		warm  bool
		cool  bool
	}{
		{time.Minute, GroupIdle, true, false},
		{AgyWarmFor, GroupIdle, true, true},
		{AgyColdAfter - time.Second, GroupIdle, true, true},
		{AgyColdAfter, GroupCold, false, false},
		{3 * time.Hour, GroupCold, false, false},
	} {
		now := at(0).Add(c.after)
		snap, _ := compute(obs(), nil, now)
		s := find(t, snap, "agy:a1")
		if s.Group != c.group || !s.Cache.Known || !s.Cache.Fades || s.Cache.Warm(now) != c.warm || s.Cache.Cooling(now) != c.cool {
			t.Errorf("after %v: group %s cache %+v", c.after, s.Group, s.Cache)
		}
		// The snapshot changes when the estimate turns cold: a deadline.
		if c.warm && !NextDeadline(snap).Equal(at(0).Add(AgyColdAfter)) {
			t.Errorf("after %v: deadline %v", c.after, NextDeadline(snap))
		}
	}
}

// --- jails ----------------------------------------------------------------------------

func jailObs() Observation {
	base := "/user.slice/user-1000.slice/user@1000.service"
	p1, p2 := pane("%1", "@1", 1), pane("%2", "@2", 2)
	p1.Session, p1.PID = "infra", 101
	p2.Session, p2.PID = "docs", 102
	return Observation{
		Tmux: TmuxView{OK: true, ServerPID: 100, Panes: []model.TmuxPane{p1, p2}},
		Jail: JailView{Available: true, ServerCgroup: base + "/cccp.slice/cccp-tmux.scope",
			PaneCgroups: map[string]string{
				"%1": base + "/cccp.slice/cccp-ws.slice/cccp-ws-infra.slice/cccp-pane-1.scope",
				"%2": base + "/cccp.slice/tmux-spawn-x.scope", // a new pane
			},
			Stats: map[string]jail.Stats{"cccp-ws-infra.slice": {Memory: 1 << 30}}},
	}
}

func TestJailState(t *testing.T) {
	snap, _ := compute(jailObs(), nil, at(1))
	j := snap.Jail
	if !j.On || j.Root != jail.Defaults.Root || len(j.Workstreams) != 2 {
		t.Fatalf("%+v", j)
	}
	infra, _ := j.Find("infra")
	if infra.Preset != jail.PresetNormal || infra.Slice != "cccp-ws-infra.slice" || infra.Stats.Memory != 1<<30 || infra.Boosted {
		t.Fatalf("%+v", infra)
	}
	if len(j.Moves) != 1 || j.Moves[0].Pane != "%2" || j.Moves[0].Slice != "cccp-ws-docs.slice" {
		t.Fatalf("moves %+v", j.Moves)
	}
	if j.Desired["cccp-ws-docs.slice"] != jail.Defaults.Presets[jail.PresetNormal] {
		t.Fatalf("desired %+v", j.Desired)
	}
}

func TestJailOff(t *testing.T) {
	c := cfg
	c.Jail.Enabled = false
	snap, _ := Compute(jailObs(), NewMemory(), c, t0, at(1))
	if snap.Jail.On || !strings.Contains(snap.Jail.Why, "enabled = false") || len(snap.Jail.Moves) != 0 {
		t.Fatalf("%+v", snap.Jail)
	}
	o := jailObs()
	o.Jail = JailView{Err: "no memory controller"}
	snap, _ = compute(o, nil, at(1))
	if snap.Jail.On || snap.Jail.Why != "no memory controller" {
		t.Fatalf("%+v", snap.Jail)
	}
}

func TestBoostsShowExpireAndSetTheDeadline(t *testing.T) {
	mem := NewMemory()
	mem.Initialized = true
	mem.Boosts["infra"] = jail.Boost{Preset: jail.PresetMax, Until: at(60)}
	mem.Boosts["gone"] = jail.Boost{Preset: jail.PresetBoost} // a workstream not running: kept
	snap, next := compute(jailObs(), &mem, at(1))
	infra, _ := snap.Jail.Find("infra")
	if infra.Preset != jail.PresetMax || !infra.Boosted || !infra.Until.Equal(at(60)) || snap.Jail.Root != jail.Defaults.RootMax {
		t.Fatalf("max boost: %+v root %+v", infra, snap.Jail.Root)
	}
	// What ending the boost returns it to.
	if infra.NormalPreset != jail.PresetNormal || infra.Normal != jail.Defaults.Presets[jail.PresetNormal] {
		t.Fatalf("normal under the boost: %+v", infra)
	}
	c := cfg
	c.Jail.Workstreams = []config.JailWorkstream{{Name: "infra", Limits: jail.Limits{MemoryMax: "64M"}}}
	s2, _ := Compute(jailObs(), mem, c, t0, at(1))
	if w, _ := s2.Jail.Find("infra"); w.NormalPreset != "custom" || w.Normal.MemoryMax != "64M" || w.Preset != jail.PresetMax {
		t.Fatalf("custom normal under a boost: %+v", w)
	}
	if d := NextDeadline(snap); !d.Equal(at(60)) {
		t.Fatalf("deadline %v", d)
	}
	snap, next = compute(jailObs(), &next, at(60))
	if infra, _ := snap.Jail.Find("infra"); infra.Boosted || infra.Preset != jail.PresetNormal || snap.Jail.Root != jail.Defaults.Root {
		t.Fatalf("expired: %+v", infra)
	}
	if _, ok := next.Boosts["infra"]; ok {
		t.Fatal("an expired boost stays in memory")
	}
	if _, ok := next.Boosts["gone"]; !ok {
		t.Fatal("an until-undone boost of a workstream not running was dropped")
	}
}

// The transcript says who started the last turn; a prompt Claude Code sent
// itself, left in hook state written before such prompts were skipped, is
// not shown as the user's.
func TestAutomaticTurnAndLegacyPrompt(t *testing.T) {
	o := yourTurnObs(false)
	o.Hooks[0].State.LastPrompt = "<task-notification> <task-id>b1</task-id>"
	o.ClaudeTranscripts = map[string]ClaudeTranscript{"s1": {Path: "/t", Meta: model.ClaudeMeta{
		LastPrompt: "dispatch the agents", LastPromptAt: at(1), LastTurnAuto: true, LastTurnAt: at(5)}}}
	snap, _ := compute(o, nil, at(11))
	s := find(t, snap, "claude:s1")
	if !s.AutoTurn || s.LastPrompt != "dispatch the agents" {
		t.Fatalf("%+v", s)
	}
	o.ClaudeTranscripts["s1"] = ClaudeTranscript{Path: "/t", Meta: model.ClaudeMeta{LastPrompt: "x", LastPromptAt: at(1)}}
	if s := find(t, func() Snapshot { s, _ := compute(o, nil, at(11)); return s }(), "claude:s1"); s.AutoTurn {
		t.Fatal("a turn the user started")
	}
}

// A summary is shown only for the state it was written for.
func TestSummaryOnlyForItsOwnState(t *testing.T) {
	o := yourTurnObs(false)
	o.Hooks[0].State.LastMessage = "Second answer."
	snap, _ := compute(o, nil, at(11))
	s := find(t, snap, "claude:s1")
	_, hash := SummaryInput(s)
	mem := NewMemory()
	mem.Initialized = true
	mem.Summaries["claude:s1"] = SummaryRecord{InputHash: "an earlier turn", Text: "old"}
	if s := find(t, func() Snapshot { s, _ := compute(o, &mem, at(11)); return s }(), "claude:s1"); s.Summary != "" {
		t.Fatalf("an earlier turn's summary: %q", s.Summary)
	}
	mem.Summaries["claude:s1"] = SummaryRecord{InputHash: hash, Text: "new", Verdict: VerdictWait}
	if s := find(t, func() Snapshot { s, _ := compute(o, &mem, at(11)); return s }(), "claude:s1"); s.Summary != "new" || s.SummaryVerdict != VerdictWait {
		t.Fatalf("its own summary: %+v", s)
	}
}

// "shell": idle with a background shell running. Flagged only while idle,
// and in WORKING only when the turn's summary says nothing is needed yet.
func TestBackgroundShell(t *testing.T) {
	o := yourTurnObs(false)
	o.Registry = []RegistryRecord{regRec("s1", 42, "shell", at(11), nil)}
	snap, _ := compute(o, nil, at(12))
	s := find(t, snap, "claude:s1")
	if !s.BackgroundShell || s.Group != GroupYourTurn || !s.Background() {
		t.Fatalf("shell, no summary yet: %+v", s)
	}
	_, hash := SummaryInput(s)
	for verdict, want := range map[string]Group{VerdictWait: GroupWorking, VerdictAct: GroupYourTurn, "": GroupYourTurn} {
		mem := NewMemory()
		mem.Initialized = true
		mem.Summaries["claude:s1"] = SummaryRecord{InputHash: hash, Text: "x", Verdict: verdict}
		if s := find(t, func() Snapshot { s, _ := compute(o, &mem, at(12)); return s }(), "claude:s1"); s.Group != want {
			t.Errorf("verdict %q: %s, want %s", verdict, s.Group, want)
		}
	}
	// A newer hook says it is working again: no longer idle, not flagged.
	o.Hooks[0].State.Activity, o.Hooks[0].State.ActivityAt = model.Working, at(13)
	if s := find(t, func() Snapshot { s, _ := compute(o, nil, at(14)); return s }(), "claude:s1"); s.BackgroundShell {
		t.Fatalf("working: %+v", s)
	}
}

// An interrupted agy turn: no Stop hook (agy runs none on an interrupt),
// but agy's database records the execution's end, and the transcript has
// nothing after it.
func TestAgyInterruptEndsTheTurnWithoutAHook(t *testing.T) {
	obs := func(lastStep int) Observation {
		return Observation{
			Hooks: []HookRecord{hookRec("a1", 42, true, func(s *model.HookState) {
				s.Agent, s.Pane = model.Agy, "%3"
				s.Activity, s.ActivityAt, s.LastEventAt = model.Working, at(100), at(100)
				s.Tool, s.TurnEndedAt, s.StartedAt = "run_command", at(10), at(1)
				s.LastPrompt, s.LastPromptAt = "continue the simulations", at(20) // after that turn end: seen
			})},
			Tmux: TmuxView{OK: true, Panes: []model.TmuxPane{pane("%3", "@7", 2), pane("%9", "@8", 3)},
				Clients: []model.TmuxClient{reporting("c", "%9", true)}},
			AgyTranscripts: map[string]AgyTranscript{"a1": {Path: "/t", Meta: model.AgyMeta{LastStepIndex: lastStep, LastStepAt: at(99)},
				Exec: model.AgyExecution{Known: true, Reason: model.AgyUserCanceled, LastStep: 690}}},
		}
	}
	// Within AgySettle of the last hook: still working, recheck when settled.
	snap, mem := compute(obs(690), nil, at(105))
	s := find(t, snap, "agy:a1")
	if s.Activity != model.Working || !s.RecheckAt.Equal(at(100).Add(AgySettle)) || !NextDeadline(snap).Equal(s.RecheckAt) {
		t.Fatalf("settling: %+v", s)
	}
	// Settled: the turn is over; an interrupt is not a completion.
	snap, _ = compute(obs(690), &mem, at(111))
	s = find(t, snap, "agy:a1")
	if s.Activity != model.Idle || s.Group == GroupYourTurn || s.Tool != "" || !s.ActivitySince.Equal(at(100)) || !s.RecheckAt.IsZero() {
		t.Fatalf("settled: %+v", s)
	}
	if len(s.Notes) != 1 || s.Notes[0] != "interrupted" {
		t.Fatalf("notes %v", s.Notes)
	}
	// A step after the execution's last: a new execution runs.
	snap, _ = compute(obs(691), &mem, at(200))
	if s := find(t, snap, "agy:a1"); s.Activity != model.Working {
		t.Fatalf("a new execution: %+v", s)
	}
	// A normal end the hooks already reported, then a background task's
	// late PostToolUse (working again): over, and not "interrupted".
	o := obs(690)
	o.AgyTranscripts["a1"] = AgyTranscript{Path: "/t", Meta: model.AgyMeta{LastStepIndex: 690},
		Exec: model.AgyExecution{Known: true, Reason: model.AgyNoToolCall, LastStep: 690}}
	snap, _ = compute(o, &mem, at(200))
	if s := find(t, snap, "agy:a1"); s.Activity != model.Idle || len(s.Notes) != 0 {
		t.Fatalf("late PostToolUse: %+v", s)
	}
	// Only a turn the hooks call working: an error stays an error.
	o = obs(690)
	o.Hooks[0].State.Activity = model.Errored
	snap, _ = compute(o, &mem, at(200))
	if s := find(t, snap, "agy:a1"); s.Activity != model.Errored {
		t.Fatalf("errored: %+v", s)
	}
	// No record, with an empty transcript: as before.
	o = obs(0)
	o.AgyTranscripts["a1"] = AgyTranscript{Path: "/t"}
	snap, _ = compute(o, &mem, at(200))
	if s := find(t, snap, "agy:a1"); s.Activity != model.Working {
		t.Fatalf("no record, no steps: %+v", s)
	}
	// No record (an older agy, or unreadable): as before.
	o = obs(690)
	o.AgyTranscripts["a1"] = AgyTranscript{Path: "/t", Meta: model.AgyMeta{LastStepIndex: 690}}
	snap, _ = compute(o, &mem, at(200))
	if s := find(t, snap, "agy:a1"); s.Activity != model.Working {
		t.Fatalf("no record: %+v", s)
	}
}

// A conversation restored by cccp start waits in a placeholder pane: it is
// opened there (never in a second window) and moved with that window.
func TestRestoredConversationOpensInItsPlaceholder(t *testing.T) {
	tag := workstream.EncodeTag(workstream.Conversation{Agent: model.Claude, SessionID: "c1", ConfigDir: "/h/.claude", Title: "Title c1"})
	ph := model.TmuxPane{ID: "%7", Session: "cam", WindowID: "@4", WindowIndex: 0, WindowPanes: 1, Path: "/w/proj", Resume: tag}
	obs := Observation{Profiles: profiles, Conversations: []Conversation{conv(model.Claude, "c1", 50), conv(model.Claude, "c2", 40)},
		Tmux: TmuxView{OK: true, Panes: []model.TmuxPane{ph, {ID: "%8", Session: "vector", WindowID: "@5", Path: "/elsewhere"}}}}
	snap, _ := compute(obs, nil, at(60))
	s := find(t, snap, "claude:c1")
	if s.Alive || s.Placeholder != "%7" || s.TmuxSess != "cam" || s.WindowID != "@4" || s.Location != "cam:0" {
		t.Fatalf("%+v", s)
	}
	if j := PlanJump(s, obs.Tmux.Panes); j.Kind != JumpPlaceholder || j.Pane != "%7" {
		t.Fatalf("jump %+v", j)
	}
	if m := PlanMove(s, "other"); m.Kind != MoveWindow || m.Window != "@4" {
		t.Fatalf("move %+v", m)
	}
	if m := PlanMove(s, "cam"); m.Kind != MoveImpossible {
		t.Fatalf("move into its own workstream: %+v", m)
	}
	// A conversation without a placeholder resumes in a new window as before.
	if s2 := find(t, snap, "claude:c2"); s2.Placeholder != "" || PlanJump(s2, obs.Tmux.Panes).Kind != JumpNewWindow {
		t.Fatalf("%+v", s2)
	}
	// Once resumed (claude runs under the placeholder), it is a live session
	// in that pane, not a waiting one.
	obs.Hooks = []HookRecord{hookRec("c1", 42, true, func(st *model.HookState) {
		st.Pane, st.Activity, st.ActivityAt = "%7", model.Working, at(55)
	})}
	snap, _ = compute(obs, nil, at(60))
	if s := find(t, snap, "claude:c1"); !s.Alive || s.Pane != "%7" || s.Placeholder != "" || PlanJump(s, nil).Kind != JumpPane {
		t.Fatalf("resumed: %+v", s)
	}
}
