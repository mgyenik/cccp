package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"cccp/internal/core"
	"cccp/internal/model"
	"cccp/internal/search"
)

// --- fakes -----------------------------------------------------------------------

type fakeSource struct{ refreshed int }

func (f *fakeSource) Start(func(core.Snapshot)) {}
func (f *fakeSource) Refresh()                  { f.refreshed++ }
func (f *fakeSource) Daemon() bool              { return true }
func (f *fakeSource) Close()                    {}

type recorder struct {
	jumped  []string
	aliases map[string]string
	seen    []string
	fail    error
}

func (r *recorder) actions() Actions {
	return Actions{
		Jump:  func(s core.Session) error { r.jumped = append(r.jumped, s.Key); return r.fail },
		Alias: func(k, v string) error { r.aliases[k] = v; return nil },
		Seen:  func(k string) error { r.seen = append(r.seen, k); return nil },
	}
}

type fakeSearch struct{ hits []search.Hit }

func (f fakeSearch) Update() (int, error) { return 0, nil }
func (f fakeSearch) Run(q search.Query, limit int) ([]search.Hit, error) {
	return f.hits, nil
}

func snapshot() core.Snapshot {
	return core.Snapshot{At: time.Now(), Sessions: []core.Session{
		{Key: "claude:a", Title: "Approve deploy", Group: core.GroupNeedsYou, Activity: model.NeedsApproval, Alive: true, Pane: "%1", Cwd: "/w/infra"},
		{Key: "claude:b", Title: "Gain staging", Group: core.GroupYourTurn, Activity: model.Idle, Alive: true, Pane: "%2", Cwd: "/w/synth"},
		{Key: "claude:c", Title: "Refactor parser", Group: core.GroupWorking, Activity: model.Working, Alive: true, Pane: "%3", Cwd: "/w/lang"},
		{Key: "agy:d", Title: "Logger console", Group: core.GroupIdle, Activity: model.Idle, Alive: true, Pane: "%4", Cwd: "/w/logger"},
		{Key: "claude:e", Title: "Old thing", Group: core.GroupEnded, Activity: model.Ended, Cwd: "/w/old"},
	}}
}

func newModel(t *testing.T) (*Model, *recorder, *fakeSource) {
	t.Helper()
	r := &recorder{aliases: map[string]string{}}
	src := &fakeSource{}
	m := New(Options{Source: src, Actions: r.actions(), Search: fakeSearch{hits: []search.Hit{
		{Doc: search.Doc{Agent: model.Claude, SessionID: "old1", Title: "Viterbi", Cwd: "/w"}, Role: "user", Snippet: "explain viterbi"},
	}}})
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m.Update(snapMsg(snapshot()))
	return m, r, src
}

// key sends one key press; for enter it also runs the returned command and
// feeds its result back (enough for jump and search).
func key(m *Model, k string) {
	var msg tea.KeyPressMsg
	switch k {
	case "enter":
		msg = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		msg = tea.KeyPressMsg{Code: tea.KeyTab}
	case "down":
		msg = tea.KeyPressMsg{Code: tea.KeyDown}
	default:
		msg = tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
	}
	_, cmd := m.Update(msg)
	// Only enter produces commands whose results matter (jump, search);
	// other commands are timers such as the cursor blink, which would sleep.
	if k == "enter" {
		run(m, cmd)
	}
}

func run(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			run(m, c)
		}
	case doneMsg, searchMsg:
		m.Update(msg)
	}
}

func typeText(m *Model, s string) {
	for _, r := range s {
		key(m, string(r))
	}
}

func selected(t *testing.T, m *Model) string {
	t.Helper()
	s, ok := m.selected()
	if !ok {
		t.Fatal("nothing selected")
	}
	return s.Key
}

// --- tests -------------------------------------------------------------------------

func TestEndedHiddenUntilToggled(t *testing.T) {
	m, _, _ := newModel(t)
	if n := len(m.visible()); n != 4 {
		t.Fatalf("visible %d", n)
	}
	key(m, "e")
	if n := len(m.visible()); n != 5 {
		t.Fatalf("with ended: %d", n)
	}
}

func TestNavigationAndJump(t *testing.T) {
	m, r, _ := newModel(t)
	if selected(t, m) != "claude:a" {
		t.Fatal("first row should be selected")
	}
	key(m, "j")
	key(m, "j")
	if selected(t, m) != "claude:c" {
		t.Fatalf("after j j: %s", selected(t, m))
	}
	key(m, "tab") // next needing attention, wrapping
	if selected(t, m) != "claude:a" {
		t.Fatalf("tab: %s", selected(t, m))
	}
	key(m, "enter")
	if len(r.jumped) != 1 || r.jumped[0] != "claude:a" || !m.quitting {
		t.Fatalf("jumped %v quitting %v", r.jumped, m.quitting)
	}
}

func TestFailedJumpKeepsCockpitOpen(t *testing.T) {
	m, r, _ := newModel(t)
	r.fail = errors.New("pane is gone")
	key(m, "enter")
	if m.quitting || !strings.Contains(m.status, "pane is gone") {
		t.Fatalf("quitting=%v status=%q", m.quitting, m.status)
	}
}

func TestSelectionFollowsSessionAcrossSnapshots(t *testing.T) {
	m, _, _ := newModel(t)
	key(m, "j") // claude:b
	snap := snapshot()
	snap.Sessions[0], snap.Sessions[1] = snap.Sessions[1], snap.Sessions[0] // b moves to the top
	m.Update(snapMsg(snap))
	if selected(t, m) != "claude:b" {
		t.Fatalf("selection moved to %s", selected(t, m))
	}
}

func TestFilter(t *testing.T) {
	m, _, _ := newModel(t)
	key(m, "/")
	typeText(m, "synth")
	if v := m.visible(); len(v) != 1 || v[0].Key != "claude:b" {
		t.Fatalf("filter: %v", v)
	}
	key(m, "esc")
	if len(m.visible()) != 4 || m.filter.Value() != "" {
		t.Fatal("esc should clear the filter")
	}
	// Filtering finds ended sessions too.
	key(m, "/")
	typeText(m, "old")
	if v := m.visible(); len(v) != 1 || v[0].Key != "claude:e" {
		t.Fatalf("ended not found by filter: %v", v)
	}
}

func TestAliasAndSeen(t *testing.T) {
	m, r, _ := newModel(t)
	key(m, "a")
	typeText(m, "infra")
	key(m, "enter")
	if r.aliases["claude:a"] != "infra" {
		t.Fatalf("aliases %v", r.aliases)
	}
	key(m, "m")
	if len(r.seen) != 1 || r.seen[0] != "claude:a" {
		t.Fatalf("seen %v", r.seen)
	}
}

func TestSearchOpensHit(t *testing.T) {
	m, r, _ := newModel(t)
	key(m, "?")
	typeText(m, "viterbi")
	key(m, "enter")
	if m.mode != modeSearchResults || len(m.hits) != 1 {
		t.Fatalf("mode %v hits %d", m.mode, len(m.hits))
	}
	key(m, "enter")
	if len(r.jumped) != 1 || r.jumped[0] != "claude:old1" {
		t.Fatalf("jumped %v", r.jumped)
	}
}

func TestRefreshAndPreviewCycle(t *testing.T) {
	m, _, src := newModel(t)
	key(m, "r")
	if src.refreshed != 1 {
		t.Fatal("refresh not requested")
	}
	s, _ := m.selected()
	if m.effectivePreview(s) != previewPane {
		t.Fatal("a live session previews its pane by default")
	}
	key(m, "p")
	if m.effectivePreview(s) != previewConversation {
		t.Fatal("p: pane → conversation")
	}
	key(m, "p")
	if m.effectivePreview(s) != previewOff {
		t.Fatal("p: conversation → off")
	}
	key(m, "p")
	if m.effectivePreview(s) != previewPane {
		t.Fatal("p: off → pane")
	}
}

func TestRenderFitsTheWindow(t *testing.T) {
	m, _, _ := newModel(t)
	for _, w := range []int{60, 100, 160, 220} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		out := m.render()
		lines := strings.Split(out, "\n")
		if len(lines) > 30 {
			t.Errorf("width %d: %d lines for a 30-line window", w, len(lines))
		}
		if !strings.Contains(out, "Approve deploy") {
			t.Errorf("width %d: first session not rendered", w)
		}
	}
}

type fakePanes struct{ captured []string }

func (f *fakePanes) CapturePane(pane string, ansi bool, lines int) (string, error) {
	f.captured = append(f.captured, pane)
	if pane == "%2" {
		return "", errors.New("can't find pane")
	}
	return "$ make\nbuilding " + pane + "\n", nil
}

func TestLivePanePreview(t *testing.T) {
	panes := &fakePanes{}
	m := New(Options{Source: &fakeSource{}, Panes: panes})
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	_, cmd := m.Update(snapMsg(snapshot()))
	if cmd == nil {
		t.Fatal("a new snapshot should load the selected pane's preview")
	}
	m.Update(cmd())
	if len(panes.captured) != 1 || panes.captured[0] != "%1" {
		t.Fatalf("captured %v", panes.captured)
	}
	if out := m.render(); !strings.Contains(out, "building %1") {
		t.Fatal("captured pane not shown in the preview")
	}
	// Moving the selection captures the new pane; a failure is shown, not fatal.
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	m.Update(cmd())
	if out := m.render(); !strings.Contains(out, "pane unavailable") {
		t.Fatal("capture error not shown")
	}
	// A late result for a previously selected pane is ignored.
	m.Update(previewMsg{key: "claude:a", text: "stale"})
	if strings.Contains(m.render(), "stale") {
		t.Fatal("stale preview shown for the wrong session")
	}
	// The conversation preview needs no capture.
	panes.captured = nil
	key(m, "p")
	if len(panes.captured) != 0 {
		t.Fatalf("conversation preview captured %v", panes.captured)
	}
}
