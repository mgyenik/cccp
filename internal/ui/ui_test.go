package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"cccp/internal/catalog"
	"cccp/internal/core"
	"cccp/internal/model"
)

// --- fakes -----------------------------------------------------------------------

type fakeSource struct{ refreshed int }

func (f *fakeSource) Start(func(core.Snapshot)) {}
func (f *fakeSource) Refresh()                  { f.refreshed++ }
func (f *fakeSource) Daemon() bool              { return true }
func (f *fakeSource) Close()                    {}

type recorder struct {
	jumped     []string
	aliases    map[string]string
	seen       []string
	summarized []string
	fail       error
}

func (r *recorder) actions() Actions {
	return Actions{
		Jump:      func(s core.Session) error { r.jumped = append(r.jumped, s.Key); return r.fail },
		Alias:     func(k, v string) error { r.aliases[k] = v; return nil },
		Seen:      func(k string) error { r.seen = append(r.seen, k); return nil },
		Summarize: func(k string) error { r.summarized = append(r.summarized, k); return nil },
	}
}

// fakeSearch answers every query with hits, recording the queries.
type fakeSearch struct {
	hits    []catalog.Hit
	queries *[]string
}

func (f fakeSearch) Run(q catalog.Query, limit int) ([]catalog.Hit, error) {
	*f.queries = append(*f.queries, strings.Join(q.Terms, " "))
	return f.hits, nil
}

func snapshot() core.Snapshot {
	return core.Snapshot{At: time.Now(), Sessions: []core.Session{
		{Key: "claude:a", Title: "Approve deploy", Group: core.GroupNeedsYou, Activity: model.NeedsApproval, Alive: true, Pane: "%1", Cwd: "/w/infra"},
		{Key: "claude:b", Title: "Gain staging", Group: core.GroupYourTurn, Activity: model.Idle, Alive: true, Pane: "%2", Cwd: "/w/synth"},
		{Key: "claude:c", Title: "Refactor parser", Group: core.GroupWorking, Activity: model.Working, Alive: true, Pane: "%3", Cwd: "/w/lang"},
		{Key: "agy:d", Title: "Logger console", Group: core.GroupIdle, Activity: model.Idle, Alive: true, Pane: "%4", Cwd: "/w/logger"},
		{Key: "claude:e", Title: "Old thing", Group: core.GroupClosed, Activity: model.Ended, Cwd: "/w/old", Turns: 12,
			Model: "claude-opus-5"},
		{Key: "claude:old1", Title: "Decoder work", Group: core.GroupClosed, Activity: model.Ended, Cwd: "/w/dsp",
			Summary: "Built a soft-decision decoder.", FirstPrompt: "write a decoder for the radio"},
	}}
}

var queries []string

func newModel(t *testing.T) (*Model, *recorder, *fakeSource) {
	t.Helper()
	r := &recorder{aliases: map[string]string{}}
	src := &fakeSource{}
	queries = nil
	m := New(Options{Source: src, Actions: r.actions(), Search: fakeSearch{queries: &queries, hits: []catalog.Hit{
		{Doc: catalog.Doc{Agent: model.Claude, SessionID: "old1", Title: "Decoder work"}, Role: "user", Snippet: "explain viterbi",
			At: time.Now().Add(-time.Hour)},
		{Doc: catalog.Doc{Agent: model.Claude, SessionID: "e", Title: "Old thing"}, Role: "assistant",
			Snippet: "…a long preamble that a narrow column would show instead of the match, viterbi again",
			At:      time.Now()},
		{Doc: catalog.Doc{Agent: model.Claude, SessionID: "gone"}, Role: "user", Snippet: "viterbi, not in the list"},
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
	case doneMsg, searchMsg, movedMsg:
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

func TestClosedShownUntilHidden(t *testing.T) {
	m, _, _ := newModel(t)
	if n := len(m.visible()); n != 6 {
		t.Fatalf("visible %d", n)
	}
	if !strings.Contains(m.render(), "CLOSED") || !strings.Contains(m.render(), "12 turns") {
		t.Fatal("closed group, or its turn count, not rendered")
	}
	for range 4 {
		key(m, "j")
	}
	out := m.render()
	if !strings.Contains(out, "12 turns · enter resumes it") || !strings.Contains(out, "claude-opus-5") ||
		strings.Contains(out, "context") {
		t.Fatal("closed preview: turns and resume hint, the model, no empty context size")
	}
	key(m, "e")
	if n := len(m.visible()); n != 4 {
		t.Fatalf("closed hidden: %d", n)
	}
	key(m, "/")
	typeText(m, "old thing")
	if v := m.visible(); len(v) != 1 || v[0].Key != "claude:e" {
		t.Fatalf("search must find closed conversations even when hidden: %v", v)
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
	if strings.Contains(m.render(), "YOUR TURN") {
		t.Fatal("search results are one flat list, without group headers")
	}
	key(m, "esc")
	if len(m.visible()) != 6 || m.filter.Value() != "" {
		t.Fatal("esc should clear the filter")
	}
	// The search finds closed conversations, by summary and first prompt.
	key(m, "/")
	typeText(m, "radio")
	if v := m.visible(); len(v) != 1 || v[0].Key != "claude:old1" {
		t.Fatalf("closed not found by its first prompt: %v", v)
	}
}

func TestSearchRanksBestMatchFirst(t *testing.T) {
	m, _, _ := newModel(t)
	key(m, "/")
	typeText(m, "de")
	v := m.visible()
	if len(v) < 2 || v[0].Key != "claude:old1" {
		t.Fatalf("prefix match on the title should rank first: %v", keys(v))
	}
	// A scattered subsequence across long prose does not match.
	key(m, "esc")
	key(m, "/")
	typeText(m, "wdftr")
	if v := m.visible(); len(v) != 0 {
		t.Fatalf("scattered match in prose: %v", keys(v))
	}
}

func keys(ss []core.Session) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Key)
	}
	return out
}

// pause delivers the end of a typing pause, runs the text search it starts
// and feeds the result back.
func pause(m *Model) {
	_, cmd := m.Update(contentTick{query: strings.TrimSpace(m.filter.Value())})
	run(m, cmd)
}

func TestContentMatchesFollowMetadataMatches(t *testing.T) {
	m, _, _ := newModel(t)
	key(m, "/")
	typeText(m, "viterbi")
	if len(m.visible()) != 0 || len(queries) != 0 {
		t.Fatal("no metadata match, and no text search before the pause")
	}
	pause(m)
	if len(queries) != 1 || queries[0] != "viterbi" {
		t.Fatalf("queries %v", queries)
	}
	// Both sessions found in their text, newest hit first; the hit for a
	// conversation not in the list is dropped.
	if v := keys(m.visible()); len(v) != 2 || v[0] != "claude:e" || v[1] != "claude:old1" {
		t.Fatalf("content matches %v", v)
	}
	if !strings.Contains(m.render(), "“…match, viterbi again”") {
		t.Fatal("the snippet, from just before the match, is the row's detail")
	}
	// A pause for a query that is no longer typed searches nothing.
	typeText(m, "x")
	_, cmd := m.Update(contentTick{query: "viterbi"})
	run(m, cmd)
	if len(queries) != 1 {
		t.Fatalf("stale pause searched: %v", queries)
	}
	if v := m.visible(); len(v) != 0 {
		t.Fatalf("the old query's content matches still apply: %v", keys(v))
	}
	// The current query's result, then a late one for the old query: the
	// late one must not replace it.
	m.Update(searchMsg{query: "viterbix", hits: []catalog.Hit{{Doc: catalog.Doc{Agent: model.Claude, SessionID: "c"}}}})
	m.Update(searchMsg{query: "viterbi", hits: []catalog.Hit{{Doc: catalog.Doc{Agent: model.Claude, SessionID: "a"}}}})
	if v := keys(m.visible()); len(v) != 1 || v[0] != "claude:c" {
		t.Fatalf("a stale result replaced the current one: %v", v)
	}
}

func TestContentMatchesDoNotRepeatMetadataMatches(t *testing.T) {
	m, _, _ := newModel(t)
	key(m, "/")
	typeText(m, "old")
	pause(m)
	v := keys(m.visible())
	if len(v) != 2 || v[0] != "claude:e" || v[1] != "claude:old1" {
		t.Fatalf("want the title match, then the other text match once: %v", v)
	}
}

func TestSelectingAClosedConversationAsksForASummaryOnce(t *testing.T) {
	m, r, _ := newModel(t)
	move := func(k rune) {
		_, cmd := m.Update(tea.KeyPressMsg{Code: k, Text: string(k)})
		run(m, cmd)
	}
	for range 4 {
		move('j')
	}
	if selected(t, m) != "claude:e" || len(r.summarized) != 1 {
		t.Fatalf("selected %s, summarized %v", selected(t, m), r.summarized)
	}
	move('j') // old1 has a summary: nothing to ask
	move('k') // back to e: already asked
	move('j')
	move('k')

	if len(r.summarized) != 1 || r.summarized[0] != "claude:e" {
		t.Fatalf("summarized %v", r.summarized)
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

func TestEnterOpensASearchResult(t *testing.T) {
	m, r, _ := newModel(t)
	key(m, "/")
	typeText(m, "viterbi")
	pause(m)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	run(m, cmd)
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

func TestProfileTagsAndFilter(t *testing.T) {
	m, _, _ := newModel(t)
	if strings.Contains(m.render(), "acmecorp") {
		t.Fatal("no tags with a single profile")
	}
	snap := snapshot()
	snap.Sessions[0].Profile, snap.Sessions[1].Profile, snap.Sessions[2].Profile = "acmecorp", "homelab", "homelab"
	m.Update(snapMsg(snap))
	if out := m.render(); !strings.Contains(out, "acmecorp") || !strings.Contains(out, "homelab") {
		t.Fatal("profile tags not shown with several profiles")
	}
	key(m, "/")
	typeText(m, "acmecorp")
	if v := m.visible(); len(v) != 1 || v[0].Key != "claude:a" {
		t.Fatalf("filter by profile: %v", v)
	}
}
