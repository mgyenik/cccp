package catalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseQuery(t *testing.T) {
	q := ParseQuery(`Viterbi "forward backward"  EM`)
	want := []string{"viterbi", "forward backward", "em"}
	if fmt.Sprint(q.Terms) != fmt.Sprint(want) {
		t.Fatalf("%q", q.Terms)
	}
}

func TestExtractAndSearch(t *testing.T) {
	dir := t.TempDir()
	ix := Index{
		TextDir:          filepath.Join(dir, "cache", "text"),
		ClaudeConfigDirs: []string{filepath.Join(dir, "claude"), filepath.Join(dir, "claude-work")},
		AgyBrainDir:      filepath.Join(dir, "agy", "brain"),
	}
	proj := filepath.Join(dir, "claude", "projects", "-w")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(proj, "S1.jsonl")
	lines := []string{
		`{"type":"ai-title","aiTitle":"Wavetable synth","sessionId":"S1","timestamp":"2026-11-01T10:00:00Z"}`,
		`{"type":"user","sessionId":"S1","cwd":"/w","timestamp":"2026-11-01T10:00:01Z","message":{"content":"build a wavetable oscillator"}}`,
		`{"type":"user","sessionId":"S1","timestamp":"2026-11-01T10:00:02Z","message":{"content":[{"type":"tool_result","content":"wavetable secret tool output"}]}}`,
		`{"type":"assistant","sessionId":"S1","timestamp":"2026-11-01T10:00:03Z","message":{"content":[{"type":"text","text":"The oscillator now\nreads frames."},{"type":"tool_use","name":"Bash","input":{"command":"wavetable"}}]}}`,
	}
	f, _ := os.Create(p)
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
	f.Close()
	if n, err := ix.Update(); err != nil || n != 1 {
		t.Fatalf("update: %d %v", n, err)
	}
	hits, err := ix.Run(ParseQuery("oscillator"), 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits %v %v", hits, err)
	}
	h := hits[0]
	if h.Doc.Title != "Wavetable synth" || h.Doc.Cwd != "/w" || h.Matches != 2 || h.Role != "assistant" ||
		h.Doc.ConfigDir != filepath.Join(dir, "claude") {
		t.Fatalf("%+v", h)
	}
	// Tool output is not indexed.
	if hits, _ := ix.Run(ParseQuery("secret"), 10); len(hits) != 0 {
		t.Fatalf("tool output leaked into the index: %+v", hits)
	}
	// Incremental: appended text is found without rebuilding.
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprintln(f, `{"type":"user","sessionId":"S1","timestamp":"2026-11-01T11:00:00Z","message":{"content":"now add a granular mode"}}`)
	f.Close()
	if n, _ := ix.Update(); n != 1 {
		t.Fatalf("incremental update count %d", n)
	}
	if hits, _ := ix.Run(ParseQuery(`"granular mode"`), 10); len(hits) != 1 {
		t.Fatal("appended text not found")
	}
	if n, _ := ix.Update(); n != 0 {
		t.Fatalf("no-op update reported %d changes", n)
	}
}

func TestSnippetCentresOnMatch(t *testing.T) {
	text := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa NEEDLE bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	s := snippet(text, "needle", 30)
	if !strings.Contains(s, "NEEDLE") || !strings.HasPrefix(s, "…") {
		t.Fatalf("%q", s)
	}
}

func TestSearchCoversEveryProfile(t *testing.T) {
	dir := t.TempDir()
	ix := Index{TextDir: filepath.Join(dir, "text"),
		ClaudeConfigDirs: []string{filepath.Join(dir, "claude"), filepath.Join(dir, "claude-work")}}
	for _, p := range []struct{ cfg, sid string }{{"claude", "personal1"}, {"claude-work", "work1"}} {
		proj := filepath.Join(dir, p.cfg, "projects", "-w")
		os.MkdirAll(proj, 0o700)
		os.WriteFile(filepath.Join(proj, p.sid+".jsonl"), []byte(`{"type":"user","timestamp":"2026-11-01T10:00:00Z","message":{"content":"shared needle"}}`+"\n"), 0o600)
	}
	ix.Update()
	hits, _ := ix.Run(ParseQuery("needle"), 10)
	got := map[string]string{}
	for _, h := range hits {
		got[h.Doc.SessionID] = filepath.Base(h.Doc.ConfigDir)
	}
	if got["personal1"] != "claude" || got["work1"] != "claude-work" {
		t.Fatalf("%v", got)
	}
}

func writeTranscript(t *testing.T, dir, cfg, sid string, lines ...string) string {
	t.Helper()
	proj := filepath.Join(dir, cfg, "projects", "-w")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(proj, sid+".jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
	f.Close()
	return p
}

func TestDocFacts(t *testing.T) {
	dir := t.TempDir()
	ix := Index{TextDir: filepath.Join(dir, "text"), ClaudeConfigDirs: []string{filepath.Join(dir, "claude")}}
	writeTranscript(t, dir, "claude", "S1",
		`{"type":"user","entrypoint":"cli","gitBranch":"main","cwd":"/w","timestamp":"2026-11-01T10:00:00Z","message":{"content":"first question"}}`,
		`{"type":"assistant","timestamp":"2026-11-01T10:00:05Z","message":{"model":"claude-x","content":[{"type":"text","text":"first answer"}]}}`,
		`{"type":"ai-title","aiTitle":"Q and A","sessionId":"S1"}`,
		`{"type":"user","gitBranch":"feature","timestamp":"2026-11-01T11:00:00Z","message":{"content":[{"type":"text","text":"second question"}]}}`,
		`{"type":"user","timestamp":"2026-11-01T11:00:01Z","message":{"content":[{"type":"tool_result","content":"x"}]}}`,
		`{"type":"assistant","timestamp":"2026-11-01T11:00:09Z","message":{"model":"claude-y","content":[{"type":"text","text":"second answer"}]}}`,
	)
	writeTranscript(t, dir, "claude", "P1",
		`{"type":"user","entrypoint":"sdk-cli","timestamp":"2026-11-01T12:00:00Z","message":{"content":"scripted"}}`)
	if _, err := ix.Update(); err != nil {
		t.Fatal(err)
	}
	docs, err := ix.Docs()
	if err != nil || len(docs) != 1 {
		t.Fatalf("print-mode runs must be excluded: %v %+v", err, docs)
	}
	d := docs[0]
	if d.Title != "Q and A" || d.FirstPrompt != "first question" || d.LastPrompt != "second question" ||
		d.LastMessage != "second answer" || d.Turns != 2 || d.Branch != "feature" || d.Model != "claude-y" ||
		d.Entrypoint != "cli" || d.Cwd != "/w" ||
		!d.First.Equal(time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)) || !d.Last.Equal(time.Date(2026, 11, 1, 11, 0, 9, 0, time.UTC)) {
		t.Fatalf("%+v", d)
	}
}

func TestOldFormatIsRebuilt(t *testing.T) {
	dir := t.TempDir()
	ix := Index{TextDir: filepath.Join(dir, "text"), ClaudeConfigDirs: []string{filepath.Join(dir, "claude")}}
	p := writeTranscript(t, dir, "claude", "S1",
		`{"type":"user","timestamp":"2026-11-01T10:00:00Z","message":{"content":"hello"}}`)
	fi, _ := os.Stat(p)
	os.MkdirAll(ix.TextDir, 0o700)
	// A cached entry from before the facts existed, claiming to be up to date.
	old, _ := json.Marshal(map[string]any{"agent": "claude", "session_id": "S1", "source": p, "offset": fi.Size()})
	os.WriteFile(filepath.Join(ix.TextDir, "claude-S1.json"), old, 0o600)
	ix.Update()
	docs, _ := ix.Docs()
	if len(docs) != 1 || docs[0].FirstPrompt != "hello" || docs[0].Version != Version {
		t.Fatalf("old format not rebuilt: %+v", docs)
	}
}

func TestConcurrentIncrementalUpdatesDoNotDuplicateText(t *testing.T) {
	// The race that matters: two updaters (daemon and cockpit) read the same
	// offset of a growing transcript and would both append the new text.
	dir := t.TempDir()
	ix := Index{TextDir: filepath.Join(dir, "text"), ClaudeConfigDirs: []string{filepath.Join(dir, "claude")}}
	pad := strings.Repeat("x", 400)
	prompts := func(from, to int) []string {
		var l []string
		for i := from; i < to; i++ {
			l = append(l, fmt.Sprintf(`{"type":"user","timestamp":"2026-11-01T10:00:00Z","message":{"content":"prompt %d %s"}}`, i, pad))
		}
		return l
	}
	writeTranscript(t, dir, "claude", "S1", prompts(0, 10)...)
	if _, err := ix.Update(); err != nil { // built once, alone
		t.Fatal(err)
	}
	const total = 4010
	writeTranscript(t, dir, "claude", "S1", prompts(10, total)...)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; ix.Update() }()
	}
	close(start)
	wg.Wait()
	b, _ := os.ReadFile(filepath.Join(ix.TextDir, "claude-S1.txt"))
	if got := strings.Count(string(b), "\n"); got != total {
		t.Fatalf("%d records for %d prompts: concurrent updates appended twice", got, total)
	}
	docs, _ := ix.Docs()
	if docs[0].Turns != total {
		t.Fatalf("turns %d", docs[0].Turns)
	}
}
