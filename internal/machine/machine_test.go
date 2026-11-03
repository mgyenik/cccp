package machine

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"cccp/internal/config"
	"cccp/internal/core"
	"cccp/internal/model"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

// scene describes one Claude session in pane %1 (window @1), with the user
// looking at pane %9 (window @9) unless visible.
type scene struct {
	activity  model.Activity
	since     int // seconds
	turnEnded int // seconds; 0 = never
	question  string
	message   string
	visible   bool
	tmuxDown  bool
	ended     bool
	battery   bool
	lastResp  int // transcript: last response, seconds (cache)
}

func (sc scene) obs() core.Observation {
	st := model.HookState{Version: 1, Agent: model.Claude, SessionID: "s1", PID: 42, PIDStart: 1, Pane: "%1",
		Activity: sc.activity, ActivityAt: at(sc.since), Question: sc.question, LastMessage: sc.message,
		StartedAt: at(1), LastEventAt: at(sc.since), Cwd: "/w/proj"}
	if sc.turnEnded > 0 {
		st.TurnEndedAt = at(sc.turnEnded)
	}
	if sc.ended {
		st.Activity, st.EndedAt = model.Ended, at(sc.since)
	}
	focus := "%9"
	if sc.visible {
		focus = "%1"
	}
	o := core.Observation{
		Hooks:     []core.HookRecord{{Path: "/state/s1.json", State: st, ProcAlive: !sc.ended}},
		OnBattery: sc.battery,
		Profiles:  testProfiles,
		Tmux: core.TmuxView{OK: !sc.tmuxDown,
			Panes: []model.TmuxPane{
				{ID: "%1", Session: "s", WindowID: "@1", WindowIndex: 1, WindowPanes: 1},
				{ID: "%9", Session: "s", WindowID: "@9", WindowIndex: 9, WindowPanes: 1},
			},
			Clients: []model.TmuxClient{{Name: "c", Pane: focus, Focused: true, Created: t0, FocusSince: t0}}},
	}
	if sc.lastResp > 0 {
		o.ClaudeTranscripts = map[string]core.ClaudeTranscript{"s1": {Path: "/t", Meta: model.ClaudeMeta{
			LastAssistantAt: at(sc.lastResp), TTL: model.TTL5m}}}
	}
	return o
}

var testProfiles = []model.Profile{
	{Name: "personal", Dir: "/h/.claude", Default: true},
	{Name: "work", Dir: "/h/.claude-work"},
}

func testConfig() config.Config {
	c := config.Default()
	c.NotifyCommand = []string{"notify", "--app"}
	c.Summaries = "off"
	return c
}

func newMachine(c config.Config) *Machine {
	mem := core.NewMemory()
	mem.Initialized = true
	return New(c, mem, t0)
}

func of[T Effect](effs []Effect) []T {
	var out []T
	for _, e := range effs {
		if x, ok := e.(T); ok {
			out = append(out, x)
		}
	}
	return out
}

func published(t *testing.T, effs []Effect) core.Snapshot {
	t.Helper()
	p := of[Publish](effs)
	if len(p) != 1 {
		t.Fatalf("want one Publish, got %d in %v", len(p), effs)
	}
	return p[0].Snapshot
}

func session(t *testing.T, snap core.Snapshot) core.Session {
	t.Helper()
	for _, s := range snap.Sessions {
		if s.Key == "claude:s1" {
			return s
		}
	}
	t.Fatalf("no claude:s1 in %+v", snap)
	return core.Session{}
}

// --- notifications -------------------------------------------------------------

func TestExistingStatesNeverNotify(t *testing.T) {
	m := newMachine(testConfig())
	effs := m.Step(Observed{Obs: scene{activity: model.NeedsApproval, since: 5, question: "Allow Bash: make"}.obs(), At: at(10)})
	if n := of[Notify](effs); len(n) != 0 {
		t.Fatalf("a state present at start notified: %v", n)
	}
	// Nor on any later observation of the same state.
	again := m.Step(Observed{Obs: scene{activity: model.NeedsApproval, since: 5, question: "Allow Bash: make"}.obs(), At: at(11)})
	if n := of[Notify](again); len(n) != 0 {
		t.Fatalf("a state present at start notified later: %v", n)
	}
	if !published(t, effs).FromDaemon {
		t.Fatal("snapshot not marked as from the daemon")
	}
}

func TestNewAttentionNotifiesOnceWhileHidden(t *testing.T) {
	m := newMachine(testConfig())
	m.Step(Observed{Obs: scene{activity: model.Working, since: 5}.obs(), At: at(6)})
	wait := scene{activity: model.NeedsApproval, since: 8, question: "Allow Bash: make install"}
	effs := m.Step(Observed{Obs: wait.obs(), At: at(9)})
	n := of[Notify](effs)
	if len(n) != 1 {
		t.Fatalf("want one notification, got %v", effs)
	}
	argv := n[0].Argv
	if !reflect.DeepEqual(argv[:2], []string{"notify", "--app"}) ||
		!strings.Contains(argv[2], "NEEDS YOU") || !strings.Contains(argv[3], "Allow Bash: make install") ||
		!strings.HasPrefix(argv[3], "s:1") || n[0].Key != "claude:s1" {
		t.Fatalf("argv %q", argv)
	}
	if len(of[Log](effs)) == 0 {
		t.Fatal("notifications must be logged")
	}
	// The same state again: no repeat.
	if n := of[Notify](m.Step(Observed{Obs: wait.obs(), At: at(12)})); len(n) != 0 {
		t.Fatalf("repeated: %v", n)
	}
	// Back to work, then a new wait: notifies again.
	m.Step(Observed{Obs: scene{activity: model.Working, since: 20}.obs(), At: at(21)})
	wait.since = 25
	if n := of[Notify](m.Step(Observed{Obs: wait.obs(), At: at(26)})); len(n) != 1 {
		t.Fatalf("new wait did not notify: %v", n)
	}
}

func TestFinishedTurnNotifiesYourTurn(t *testing.T) {
	m := newMachine(testConfig())
	m.Step(Observed{Obs: scene{activity: model.Working, since: 5}.obs(), At: at(6)})
	effs := m.Step(Observed{Obs: scene{activity: model.Idle, since: 9, turnEnded: 9, message: "Done. Ship it?"}.obs(), At: at(10)})
	n := of[Notify](effs)
	if len(n) != 1 || !strings.Contains(n[0].Argv[2], "YOUR TURN") || !strings.Contains(n[0].Argv[3], "Ship it?") {
		t.Fatalf("%v", effs)
	}
}

func TestNoNotificationWhenVisibleOrDisabled(t *testing.T) {
	m := newMachine(testConfig())
	m.Step(Observed{Obs: scene{activity: model.Working, since: 5, visible: true}.obs(), At: at(6)})
	if n := of[Notify](m.Step(Observed{Obs: scene{activity: model.NeedsApproval, since: 8, visible: true}.obs(), At: at(9)})); len(n) != 0 {
		t.Fatalf("visible session notified: %v", n)
	}

	c := testConfig()
	c.Notify = false
	m = newMachine(c)
	m.Step(Observed{Obs: scene{activity: model.Working, since: 5}.obs(), At: at(6)})
	if n := of[Notify](m.Step(Observed{Obs: scene{activity: model.NeedsApproval, since: 8}.obs(), At: at(9)})); len(n) != 0 {
		t.Fatalf("notify=false notified: %v", n)
	}
}

// --- tmux decoration -------------------------------------------------------------

func TestDecorationOnlyOnChange(t *testing.T) {
	m := newMachine(testConfig())
	effs := m.Step(Observed{Obs: scene{activity: model.Working, since: 5}.obs(), At: at(6)})
	st := of[SetStatus](effs)
	gl := of[SetGlyph](effs)
	if len(st) != 1 || !strings.Contains(st[0].Value, "●1") || len(gl) != 1 || gl[0].Window != "@1" || !strings.Contains(gl[0].Value, "●") {
		t.Fatalf("status %v glyphs %v", st, gl)
	}
	// Unchanged: nothing re-sent.
	effs = m.Step(Observed{Obs: scene{activity: model.Working, since: 5}.obs(), At: at(7)})
	if len(of[SetStatus](effs))+len(of[SetGlyph](effs)) != 0 {
		t.Fatalf("unchanged decoration re-sent: %v", effs)
	}
	// Session ends: the status empties and the glyph is cleared.
	effs = m.Step(Observed{Obs: scene{activity: model.Working, since: 8, ended: true}.obs(), At: at(9)})
	if st := of[SetStatus](effs); len(st) != 1 || st[0].Value != "" {
		t.Fatalf("status %v", st)
	}
	if gl := of[SetGlyph](effs); len(gl) != 1 || gl[0] != (SetGlyph{Window: "@1", Value: ""}) {
		t.Fatalf("glyphs %v", gl)
	}
}

func TestDecorationResentAfterTmuxRestartOrFailure(t *testing.T) {
	m := newMachine(testConfig())
	work := scene{activity: model.Working, since: 5}
	m.Step(Observed{Obs: work.obs(), At: at(6)})
	down := work
	down.tmuxDown = true
	if effs := m.Step(Observed{Obs: down.obs(), At: at(7)}); len(of[SetStatus](effs))+len(of[SetGlyph](effs)) != 0 {
		t.Fatalf("decorated a down server: %v", effs)
	}
	effs := m.Step(Observed{Obs: work.obs(), At: at(8)})
	if len(of[SetStatus](effs)) != 1 || len(of[SetGlyph](effs)) != 1 {
		t.Fatalf("restarted server not re-decorated: %v", effs)
	}
	if effs := m.Step(TmuxFailed{}); len(effs) != 0 {
		t.Fatalf("TmuxFailed has no effects of its own: %v", effs)
	}
	effs = m.Step(Observed{Obs: work.obs(), At: at(9)})
	if len(of[SetStatus](effs)) != 1 || len(of[SetGlyph](effs)) != 1 {
		t.Fatalf("not re-sent after a failure: %v", effs)
	}
}

// --- memory: seen, alias, save debounce --------------------------------------------

func TestMarkSeenClearsYourTurn(t *testing.T) {
	m := newMachine(testConfig())
	done := scene{activity: model.Idle, since: 9, turnEnded: 9}
	if s := session(t, published(t, m.Step(Observed{Obs: done.obs(), At: at(10)}))); s.Group != core.GroupYourTurn {
		t.Fatalf("%s", s.Group)
	}
	if s := session(t, published(t, m.Step(MarkSeen{Key: "claude:s1", At: at(11)}))); s.Group == core.GroupYourTurn || s.Unseen {
		t.Fatalf("still your turn after MarkSeen: %+v", s)
	}
}

func TestAliasAndSaveDebounce(t *testing.T) {
	m := newMachine(testConfig())
	m.Step(Observed{Obs: scene{activity: model.Working, since: 5}.obs(), At: at(6)})
	// The first observation wrote seen marks; saving waits SaveDelay.
	effs := m.Step(SetAlias{Key: "claude:s1", Value: "  build box ", At: at(7)})
	if s := session(t, published(t, effs)); s.Alias != "build box" || s.DisplayTitle() != "build box" {
		t.Fatalf("alias %q", s.Alias)
	}
	if len(of[SaveMemory](effs)) != 0 {
		t.Fatal("saved before the delay")
	}
	// The timer fires at the save deadline.
	effs = m.Step(TimerFired{At: at(6).Add(SaveDelay)})
	sv := of[SaveMemory](effs)
	if len(sv) != 1 || sv[0].Memory.Aliases["claude:s1"] != "build box" {
		t.Fatalf("save %v", effs)
	}
	// Nothing changed since: no second save.
	if sv := of[SaveMemory](m.Step(TimerFired{At: at(30)})); len(sv) != 0 {
		t.Fatalf("saved again without changes: %v", sv)
	}
	// Clearing the alias.
	if s := session(t, published(t, m.Step(SetAlias{Key: "claude:s1", Value: "", At: at(31)}))); s.Alias != "" {
		t.Fatal("alias not cleared")
	}
}

func TestTimerArmedForSaveThenCacheExpiry(t *testing.T) {
	m := newMachine(testConfig())
	idle := scene{activity: model.Idle, since: 5, lastResp: 5}
	effs := m.Step(Observed{Obs: idle.obs(), At: at(6)})
	arm := of[ArmTimer](effs)
	if len(arm) != 1 || !arm[0].At.Equal(at(6).Add(SaveDelay)) {
		t.Fatalf("want the save deadline first: %v", arm)
	}
	effs = m.Step(TimerFired{At: at(6).Add(SaveDelay)})
	arm = of[ArmTimer](effs)
	if len(arm) != 1 || !arm[0].At.Equal(at(5).Add(5*time.Minute)) {
		t.Fatalf("want the cache expiry next: %v", arm)
	}
	// Same deadline: not re-armed.
	if arm := of[ArmTimer](m.Step(TimerFired{At: at(60)})); len(arm) != 0 {
		t.Fatalf("re-armed unchanged deadline: %v", arm)
	}
	// After expiry the session is cold and no deadline remains.
	effs = m.Step(TimerFired{At: at(5).Add(5 * time.Minute).Add(time.Second)})
	if s := session(t, published(t, effs)); s.Group != core.GroupCold {
		t.Fatalf("%s", s.Group)
	}
	if arm := of[ArmTimer](effs); len(arm) != 1 || !arm[0].At.IsZero() {
		t.Fatalf("want the timer disarmed: %v", arm)
	}
}

// --- summaries ---------------------------------------------------------------------

func summaryConfig(policy string) config.Config {
	c := testConfig()
	c.Summaries = policy
	return c
}

var needsYou = scene{activity: model.NeedsApproval, since: 8, question: "Allow Bash: make install", message: "Ready to install."}

func TestSummaryPolicy(t *testing.T) {
	cases := []struct {
		policy  string
		battery bool
		want    int
	}{
		{"off", false, 0}, {"ac", true, 0}, {"ac", false, 1}, {"always", true, 1},
	}
	for _, c := range cases {
		m := newMachine(summaryConfig(c.policy))
		sc := needsYou
		sc.battery = c.battery
		if got := len(of[StartSummary](m.Step(Observed{Obs: sc.obs(), At: at(9)}))); got != c.want {
			t.Errorf("%s battery=%v: %d summaries, want %d", c.policy, c.battery, got, c.want)
		}
	}
}

func TestSummaryLifecycle(t *testing.T) {
	m := newMachine(summaryConfig("always"))
	ss := of[StartSummary](m.Step(Observed{Obs: needsYou.obs(), At: at(9)}))
	if len(ss) != 1 || ss[0].Key != "claude:s1" || ss[0].Model != "haiku" || !strings.Contains(ss[0].Input, "Allow Bash: make install") {
		t.Fatalf("%+v", ss)
	}
	// While it runs, more observations do not start another.
	if ss := of[StartSummary](m.Step(Observed{Obs: needsYou.obs(), At: at(10)})); len(ss) != 0 {
		t.Fatalf("second job while one runs: %v", ss)
	}
	effs := m.Step(SummaryDone{Key: ss[0].Key, Hash: ss[0].Hash, Text: "\"Approve make install\"\nextra", At: at(12)})
	if s := session(t, published(t, effs)); s.Summary != "Approve make install" {
		t.Fatalf("summary %q", s.Summary)
	}
	// Stored with its input hash: not regenerated for the same input.
	if ss := of[StartSummary](m.Step(Observed{Obs: needsYou.obs(), At: at(13)})); len(ss) != 0 {
		t.Fatalf("regenerated: %v", ss)
	}
}

func TestFailedSummaryIsNotRespawned(t *testing.T) {
	m := newMachine(summaryConfig("always"))
	ss := of[StartSummary](m.Step(Observed{Obs: needsYou.obs(), At: at(9)}))
	effs := m.Step(SummaryDone{Key: ss[0].Key, Hash: ss[0].Hash, Err: errors.New("usage limit"), At: at(10)})
	if len(of[StartSummary](effs)) != 0 {
		t.Fatalf("respawned immediately: %v", effs)
	}
	for i := 11; i < 40; i++ {
		if ss := of[StartSummary](m.Step(Observed{Obs: needsYou.obs(), At: at(i)})); len(ss) != 0 {
			t.Fatalf("respawned at %ds: %v", i, ss)
		}
	}
	if ss := of[StartSummary](m.Step(Observed{Obs: needsYou.obs(), At: at(10).Add(backoffBase + time.Second)})); len(ss) != 1 {
		t.Fatal("not retried after the backoff")
	}
}

// withSecond adds a second session, s2 in pane %2, in the same state.
func withSecond(o core.Observation) core.Observation {
	h := o.Hooks[0]
	h.Path, h.State.SessionID, h.State.PID, h.State.Pane = "/state/s2.json", "s2", 43, "%2"
	o.Hooks = append(o.Hooks, h)
	o.Tmux.Panes = append(o.Tmux.Panes, model.TmuxPane{ID: "%2", Session: "s", WindowID: "@2", WindowIndex: 2, WindowPanes: 1})
	return o
}

func TestQueuedSummaryForVanishedSessionIsDropped(t *testing.T) {
	m := newMachine(summaryConfig("always"))
	both := withSecond(needsYou.obs())
	ss := of[StartSummary](m.Step(Observed{Obs: both, At: at(9)}))
	if len(ss) != 1 {
		t.Fatalf("%v", ss)
	}
	first := ss[0].Key
	// The other session (queued behind the running job) goes away.
	only := withSecond(needsYou.obs())
	if first == "claude:s1" {
		only.Hooks = only.Hooks[:1] // keep s1, drop the queued s2
	} else {
		only.Hooks = only.Hooks[1:] // keep s2, drop the queued s1
	}
	m.Step(Observed{Obs: only, At: at(10)})
	effs := m.Step(SummaryDone{Key: first, Hash: ss[0].Hash, Text: "x", At: at(11)})
	for _, s := range of[StartSummary](effs) {
		if s.Key != first {
			t.Fatalf("started a summary for a session that no longer exists: %+v", s)
		}
	}
}

func TestCleanSummary(t *testing.T) {
	for in, want := range map[string]string{
		"  \"Approve the deploy\"  ": "Approve the deploy",
		"line one\nline two":         "line one",
		strings.Repeat("x", 200):     strings.Repeat("x", 119) + "…",
		"":                           "",
	} {
		if got := cleanSummary(in); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
}

func TestSummaryInputTailKeepsUTF8(t *testing.T) {
	s := core.Session{Title: "t", LastMessage: strings.Repeat("é", 2000)}
	in, hash := summaryInput(s)
	if !strings.Contains(in, "…") || strings.ContainsRune(in, '�') || len(hash) != 16 {
		t.Fatalf("hash %q, invalid UTF-8 or no ellipsis", hash)
	}
}

// --- pruning, stop, determinism --------------------------------------------------------

func TestPruneOnFirstObservationThenEverySixHours(t *testing.T) {
	m := newMachine(testConfig())
	old := scene{activity: model.Idle, since: 1, ended: true}.obs()
	now := t0.Add(testConfig().EndedRetention.Duration + time.Hour)
	effs := m.Step(Observed{Obs: old, At: now})
	rf := of[RemoveFiles](effs)
	if len(rf) != 1 || !reflect.DeepEqual(rf[0].Paths, []string{"/state/s1.json"}) {
		t.Fatalf("%v", effs)
	}
	if rf := of[RemoveFiles](m.Step(Observed{Obs: old, At: now.Add(time.Hour)})); len(rf) != 0 {
		t.Fatalf("pruned again within %v: %v", PruneEvery, rf)
	}
	if rf := of[RemoveFiles](m.Step(Observed{Obs: old, At: now.Add(PruneEvery + time.Second)})); len(rf) != 1 {
		t.Fatal("not pruned after PruneEvery")
	}
}

func TestStopClearsDecorationAndSaves(t *testing.T) {
	m := newMachine(testConfig())
	m.Step(Observed{Obs: scene{activity: model.Working, since: 5}.obs(), At: at(6)})
	effs := m.Step(Stop{At: at(7)})
	want := []Effect{SaveMemory{Memory: m.Memory()}, ArmTimer{}, SetStatus{Value: ""}, SetGlyph{Window: "@1", Value: ""}}
	if fmt.Sprint(effs) != fmt.Sprint(want) {
		t.Fatalf("got %v\nwant %v", effs, want)
	}
	if effs := m.Step(Observed{Obs: scene{activity: model.Working, since: 5}.obs(), At: at(8)}); effs != nil {
		t.Fatalf("stepped after Stop: %v", effs)
	}
}

func TestNoEffectsBeforeFirstObservation(t *testing.T) {
	m := newMachine(testConfig())
	for _, in := range []Input{TimerFired{At: at(1)}, MarkSeen{Key: "k", At: at(1)}, ConfigChanged{Config: testConfig(), At: at(1)}} {
		if effs := m.Step(in); len(effs) != 0 {
			t.Fatalf("%T before any observation: %v", in, effs)
		}
	}
}

func TestDeterministic(t *testing.T) {
	run := func() string {
		m := newMachine(summaryConfig("always"))
		var all []Effect
		for i, sc := range []scene{
			{activity: model.Working, since: 1},
			needsYou,
			{activity: model.Idle, since: 20, turnEnded: 20, message: "done"},
			{activity: model.Idle, since: 20, turnEnded: 20, visible: true},
		} {
			all = append(all, m.Step(Observed{Obs: sc.obs(), At: at(30 + i)})...)
		}
		return fmt.Sprint(all)
	}
	first := run()
	for i := 0; i < 10; i++ {
		if run() != first {
			t.Fatal("same inputs produced different effects")
		}
	}
}

// --- summaries and profiles ------------------------------------------------------

func TestSummaryRunsUnderTheSessionsProfile(t *testing.T) {
	m := newMachine(summaryConfig("always"))
	o := needsYou.obs()
	o.Hooks[0].State.ConfigDir = "/h/.claude-work"
	ss := of[StartSummary](m.Step(Observed{Obs: o, At: at(9)}))
	if len(ss) != 1 || ss[0].Profile.Dir != "/h/.claude-work" || ss[0].Profile.Default {
		t.Fatalf("%+v", ss)
	}
	// The default profile is named explicitly, not left to the environment.
	m = newMachine(summaryConfig("always"))
	ss = of[StartSummary](m.Step(Observed{Obs: needsYou.obs(), At: at(9)}))
	if len(ss) != 1 || ss[0].Profile.Dir != "/h/.claude" || !ss[0].Profile.Default {
		t.Fatalf("%+v", ss)
	}
}

func TestPerProfileSummaryPolicy(t *testing.T) {
	c := summaryConfig("always")
	m := newMachine(c)
	o := needsYou.obs()
	o.Hooks[0].State.ConfigDir = "/h/.claude-work"
	o.Profiles = []model.Profile{testProfiles[0], {Name: "work", Dir: "/h/.claude-work", Summaries: "off"}}
	if ss := of[StartSummary](m.Step(Observed{Obs: o, At: at(9)})); len(ss) != 0 {
		t.Fatalf("work profile has summaries off: %+v", ss)
	}
	// An "always" override beats a global "off".
	m = newMachine(summaryConfig("off"))
	o.Profiles = []model.Profile{testProfiles[0], {Name: "work", Dir: "/h/.claude-work", Summaries: "always"}}
	if ss := of[StartSummary](m.Step(Observed{Obs: o, At: at(10)})); len(ss) != 1 {
		t.Fatalf("override not applied: %+v", ss)
	}
}

func TestNoSummaryWithoutAProfile(t *testing.T) {
	m := newMachine(summaryConfig("always"))
	o := needsYou.obs()
	o.Hooks[0].State.ConfigDir = "/somewhere/unknown"
	if ss := of[StartSummary](m.Step(Observed{Obs: o, At: at(9)})); len(ss) != 0 {
		t.Fatalf("summarized under no known profile: %+v", ss)
	}
}

func TestNotificationNamesTheProfileWhenSeveralExist(t *testing.T) {
	m := newMachine(testConfig())
	m.Step(Observed{Obs: scene{activity: model.Working, since: 5}.obs(), At: at(6)})
	o := scene{activity: model.NeedsApproval, since: 8}.obs()
	o.Hooks[0].State.ConfigDir = "/h/.claude-work"
	n := of[Notify](m.Step(Observed{Obs: o, At: at(9)}))
	if len(n) != 1 || !strings.Contains(n[0].Argv[2], "[work] ") {
		t.Fatalf("%v", n)
	}
	// With a single profile, no tag.
	m = newMachine(testConfig())
	one := scene{activity: model.Working, since: 5}.obs()
	one.Profiles = testProfiles[:1]
	m.Step(Observed{Obs: one, At: at(6)})
	w := scene{activity: model.NeedsApproval, since: 8}.obs()
	w.Profiles = testProfiles[:1]
	if n := of[Notify](m.Step(Observed{Obs: w, At: at(9)})); len(n) != 1 || strings.Contains(n[0].Argv[2], "[") {
		t.Fatalf("%v", n)
	}
}

// --- closed conversations ---------------------------------------------------------

func closedObs(ids ...string) core.Observation {
	o := core.Observation{Profiles: testProfiles}
	for _, id := range ids {
		o.Conversations = append(o.Conversations, core.Conversation{Agent: model.Claude, SessionID: id,
			ConfigDir: "/h/.claude", Cwd: "/w", Title: "T " + id, FirstPrompt: "build it", LastPrompt: "ship it",
			LastMessage: "Shipped.", Last: at(1)})
	}
	return o
}

func TestLiveSummaryUsesTheLivePrompt(t *testing.T) {
	m := newMachine(summaryConfig("always"))
	ss := of[StartSummary](m.Step(Observed{Obs: needsYou.obs(), At: at(9)}))
	if len(ss) != 1 || ss[0].SystemPrompt != LiveSummaryPrompt {
		t.Fatalf("%+v", ss)
	}
}

func TestSummaryWhenASessionCloses(t *testing.T) {
	m := newMachine(summaryConfig("always"))
	live := scene{activity: model.Working, since: 5}.obs()
	live.Conversations = closedObs("s1").Conversations
	m.Step(Observed{Obs: live, At: at(6)})
	ended := scene{activity: model.Working, since: 8, ended: true}.obs()
	ended.Conversations = live.Conversations
	ss := of[StartSummary](m.Step(Observed{Obs: ended, At: at(9)}))
	if len(ss) != 1 || ss[0].Key != "claude:s1" || ss[0].SystemPrompt != ClosedSummaryPrompt ||
		!strings.Contains(ss[0].Input, "build it") || ss[0].Profile.Dir != "/h/.claude" {
		t.Fatalf("%+v", ss)
	}
}

func TestClosedConversationsAreSummarisedOnlyOnRequest(t *testing.T) {
	m := newMachine(summaryConfig("always"))
	obs := closedObs("c1", "c2", "c3")
	if ss := of[StartSummary](m.Step(Observed{Obs: obs, At: at(2)})); len(ss) != 0 {
		t.Fatalf("no bulk backfill of closed conversations: %+v", ss)
	}
	ss := of[StartSummary](m.Step(RequestSummary{Key: "claude:c2", At: at(3)}))
	if len(ss) != 1 || ss[0].Key != "claude:c2" || ss[0].SystemPrompt != ClosedSummaryPrompt {
		t.Fatalf("%+v", ss)
	}
	effs := m.Step(SummaryDone{Key: ss[0].Key, Hash: ss[0].Hash, Text: "Built and shipped the thing", At: at(4)})
	for _, s := range published(t, effs).Sessions {
		if s.Key == "claude:c2" && s.Summary != "Built and shipped the thing" {
			t.Fatalf("summary not shown: %+v", s)
		}
	}
	// Asked again with the same input: already summarised.
	if ss := of[StartSummary](m.Step(RequestSummary{Key: "claude:c2", At: at(5)})); len(ss) != 0 {
		t.Fatalf("regenerated: %+v", ss)
	}
	// A request is consumed: later observations do not summarise c1 or c3.
	if ss := of[StartSummary](m.Step(Observed{Obs: obs, At: at(6)})); len(ss) != 0 {
		t.Fatalf("%+v", ss)
	}
}
