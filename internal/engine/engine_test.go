package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cccp/internal/claudereg"
	"cccp/internal/config"
	"cccp/internal/model"
	"cccp/internal/procs"
	"cccp/internal/store"
	"cccp/internal/transcript"
)

func TestMergeActivity(t *testing.T) {
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	hook := func(a model.Activity, sec int) *model.HookState {
		return &model.HookState{Activity: a, ActivityAt: base.Add(time.Duration(sec) * time.Second), Tool: "Bash", ToolDetail: "make", Question: "q-hook"}
	}
	reg := func(status, waiting string, sec int) *claudereg.Entry {
		return &claudereg.Entry{Status: status, WaitingFor: waiting, StatusUpdatedMs: base.Add(time.Duration(sec) * time.Second).UnixMilli()}
	}
	cases := []struct {
		name     string
		h        *model.HookState
		r        *claudereg.Entry
		want     model.Activity
		wantTool string
	}{
		{"hook only", hook(model.Working, 5), nil, model.Working, "Bash"},
		{"registry only", nil, reg("waiting", "permission prompt", 5), model.NeedsApproval, ""},
		{"same class keeps hook detail", hook(model.NeedsApproval, 5), reg("waiting", "permission prompt", 6), model.NeedsApproval, "Bash"},
		{"same class: errored is a refinement of idle", hook(model.Errored, 5), reg("idle", "", 6), model.Errored, "Bash"},
		{"interrupt: newer registry idle wins", hook(model.Working, 5), reg("idle", "", 9), model.Idle, ""},
		{"manual deny: newer registry busy wins", hook(model.NeedsApproval, 5), reg("busy", "", 9), model.Working, ""},
		{"older registry loses", hook(model.Working, 9), reg("idle", "", 5), model.Working, "Bash"},
		{"unknown registry status, newer", hook(model.Working, 5), reg("weird", "", 9), model.Unknown, ""},
	}
	for _, c := range cases {
		var s Session
		mergeActivity(&s, c.h, c.r, transcript.ClaudeMeta{})
		if s.Activity != c.want || s.Tool != c.wantTool {
			t.Errorf("%s: got %s/%q want %s/%q", c.name, s.Activity, s.Tool, c.want, c.wantTool)
		}
	}
}

func TestGroupOf(t *testing.T) {
	now := time.Now()
	warm := Cache{Known: true, ExpiresAt: now.Add(time.Minute)}
	cold := Cache{Known: true, ExpiresAt: now.Add(-time.Minute)}
	cases := []struct {
		s    Session
		want Group
	}{
		{Session{Alive: true, Activity: model.NeedsApproval}, GroupNeedsYou},
		{Session{Alive: true, Activity: model.Errored}, GroupNeedsYou},
		{Session{Alive: true, Activity: model.Working}, GroupWorking},
		{Session{Alive: true, Activity: model.Idle, Unseen: true, Agent: model.Claude, Cache: cold}, GroupYourTurn},
		{Session{Alive: true, Activity: model.Idle, Agent: model.Claude, Cache: warm}, GroupIdle},
		{Session{Alive: true, Activity: model.Idle, Agent: model.Claude, Cache: cold}, GroupCold},
		{Session{Alive: true, Activity: model.Idle, Agent: model.Agy}, GroupIdle},
		{Session{Alive: true, Activity: model.Unknown}, GroupUnknown},
		{Session{Alive: false, Activity: model.Working}, GroupEnded},
	}
	for i, c := range cases {
		if g := groupOf(&c.s, now); g != c.want {
			t.Errorf("case %d: got %s want %s", i, g, c.want)
		}
	}
}

// isolate points every source at empty temp directories.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CCCP_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("CCCP_CACHE_DIR", filepath.Join(dir, "cache"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("HOME", dir)
	t.Setenv("CCCP_TMUX_SOCKET", filepath.Join(dir, "no-tmux"))
	if err := os.MkdirAll(filepath.Join(dir, "claude", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func self(t *testing.T) (int, uint64) {
	st, err := procs.ReadStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	return st.PID, st.StartTime
}

func putHook(t *testing.T, s model.HookState) {
	t.Helper()
	if err := store.Update(s.Agent, s.SessionID, func(model.HookState) model.HookState { return s }); err != nil {
		t.Fatal(err)
	}
}

func find(snap Snapshot, key string) (Session, bool) {
	for _, s := range snap.Sessions {
		if s.Key == key {
			return s, true
		}
	}
	return Session{}, false
}

func TestSnapshotYourTurnAndSeen(t *testing.T) {
	isolate(t)
	pid, start := self(t)
	e := New(config.Default(), nil)
	e.Persist.Initialized = true // not the first run: new sessions are new
	began := time.Now()

	h := model.HookState{Version: 1, Agent: model.Claude, SessionID: "s1", PID: pid, PIDStart: start,
		Activity: model.Working, ActivityAt: began.Add(time.Second), StartedAt: began.Add(time.Millisecond), LastEventAt: began.Add(time.Second)}
	putHook(t, h)
	e.LoadAll()
	snap := e.Snapshot(began.Add(2 * time.Second))
	s, ok := find(snap, "claude:s1")
	if !ok || s.Group != GroupWorking || !s.Alive {
		t.Fatalf("working: %+v", s)
	}

	h.Activity, h.ActivityAt, h.TurnEndedAt = model.Idle, began.Add(5*time.Second), began.Add(5*time.Second)
	putHook(t, h)
	e.LoadHooks()
	snap = e.Snapshot(began.Add(6 * time.Second))
	s, _ = find(snap, "claude:s1")
	if s.Group != GroupYourTurn || !s.Unseen {
		t.Fatalf("want your turn: group=%s unseen=%v seen=%v", s.Group, s.Unseen, e.Persist.Seen["claude:s1"])
	}

	e.Persist.Seen["claude:s1"] = began.Add(7 * time.Second)
	snap = e.Snapshot(began.Add(8 * time.Second))
	s, _ = find(snap, "claude:s1")
	if s.Unseen || s.Group == GroupYourTurn {
		t.Fatalf("seen session still flagged: %+v", s)
	}
}

func TestSnapshotPreexistingSessionsAreSeen(t *testing.T) {
	isolate(t)
	pid, start := self(t)
	now := time.Now()
	putHook(t, model.HookState{Version: 1, Agent: model.Claude, SessionID: "old", PID: pid, PIDStart: start,
		Activity: model.Idle, ActivityAt: now.Add(-time.Hour), TurnEndedAt: now.Add(-time.Hour),
		StartedAt: now.Add(-2 * time.Hour), LastEventAt: now.Add(-time.Hour)})
	e := New(config.Default(), nil) // first run ever
	e.LoadAll()
	s, _ := find(e.Snapshot(now), "claude:old")
	if s.Unseen {
		t.Fatal("a completion from before cccp first looked must not be flagged")
	}
	if !e.Persist.Initialized {
		t.Fatal("first snapshot should mark the state initialised")
	}
}

func TestSnapshotRegistryAndSupersede(t *testing.T) {
	dir := isolate(t)
	pid, start := self(t)
	now := time.Now()
	// Two hook sessions on one pid (a /clear without SessionEnd); the
	// registry names s-new as the pid's current session.
	putHook(t, model.HookState{Version: 1, Agent: model.Claude, SessionID: "s-old", PID: pid, PIDStart: start,
		Activity: model.Idle, ActivityAt: now.Add(-time.Minute), LastEventAt: now.Add(-time.Minute)})
	putHook(t, model.HookState{Version: 1, Agent: model.Claude, SessionID: "s-new", PID: pid, PIDStart: start,
		Activity: model.Working, ActivityAt: now.Add(-10 * time.Second), LastEventAt: now.Add(-10 * time.Second)})
	reg := map[string]any{"pid": pid, "sessionId": "s-new", "cwd": "/w", "procStart": fmt.Sprint(start),
		"kind": "interactive", "status": "idle", "statusUpdatedAt": now.Add(-time.Second).UnixMilli(), "name": "w-1", "nameSource": "derived"}
	b, _ := json.Marshal(reg)
	if err := os.WriteFile(filepath.Join(dir, "claude", "sessions", fmt.Sprintf("%d.json", pid)), b, 0o600); err != nil {
		t.Fatal(err)
	}
	e := New(config.Default(), nil)
	e.LoadAll()
	snap := e.Snapshot(now)
	old, _ := find(snap, "claude:s-old")
	cur, _ := find(snap, "claude:s-new")
	if old.Alive || old.Group != GroupEnded {
		t.Fatalf("superseded session should be ended: %+v", old)
	}
	if !cur.Alive || cur.Tracking != TrackHooksRegistry || cur.Activity != model.Idle {
		t.Fatalf("current session: tracking=%s activity=%s alive=%v", cur.Tracking, cur.Activity, cur.Alive)
	}
	// With no custom or AI title, the derived registry name (the one Claude
	// shows and `claude attach` accepts) is preferred over the bare cwd.
	if cur.Title != "w-1" {
		t.Fatalf("title %q from %s", cur.Title, cur.TitleSource)
	}
}

func TestRegistryTransitionIsCompletion(t *testing.T) {
	dir := isolate(t)
	pid, start := self(t)
	now := time.Now()
	p := filepath.Join(dir, "claude", "sessions", fmt.Sprintf("%d.json", pid))
	writeReg := func(status string, at time.Time) {
		b, _ := json.Marshal(map[string]any{"pid": pid, "sessionId": "r1", "cwd": "/w", "procStart": fmt.Sprint(start),
			"kind": "interactive", "status": status, "statusUpdatedAt": at.UnixMilli(), "startedAt": now.Add(time.Second).UnixMilli()})
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e := New(config.Default(), nil)
	e.Persist.Initialized = true
	writeReg("busy", now.Add(2*time.Second))
	e.LoadAll()
	e.Snapshot(now.Add(3 * time.Second))
	writeReg("idle", now.Add(4*time.Second))
	e.LoadRegistryFile(p)
	s, _ := find(e.Snapshot(now.Add(5*time.Second)), "claude:r1")
	if !s.Unseen || s.Group != GroupYourTurn || !s.CompletedAt.Equal(time.UnixMilli(now.Add(4*time.Second).UnixMilli())) {
		t.Fatalf("busy→idle should be a completion: %+v", s)
	}
}

func TestCleanPaneTitle(t *testing.T) {
	for in, want := range map[string]string{
		"✳ Claude Code control panel design": "Claude Code control panel design",
		"plain title":                        "plain title",
		"laptop":                             "laptop",
	} {
		if got := cleanPaneTitle(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
