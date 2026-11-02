package search

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	t.Setenv("CCCP_CACHE_DIR", filepath.Join(dir, "cache"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("HOME", dir)
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
	if n, err := Update(); err != nil || n != 1 {
		t.Fatalf("update: %d %v", n, err)
	}
	hits, err := Run(ParseQuery("oscillator"), 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits %v %v", hits, err)
	}
	h := hits[0]
	if h.Doc.Title != "Wavetable synth" || h.Doc.Cwd != "/w" || h.Matches != 2 || h.Role != "assistant" {
		t.Fatalf("%+v", h)
	}
	// Tool output is not indexed.
	if hits, _ := Run(ParseQuery("secret"), 10); len(hits) != 0 {
		t.Fatalf("tool output leaked into the index: %+v", hits)
	}
	// Incremental: appended text is found without rebuilding.
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprintln(f, `{"type":"user","sessionId":"S1","timestamp":"2026-11-01T11:00:00Z","message":{"content":"now add a granular mode"}}`)
	f.Close()
	if n, _ := Update(); n != 1 {
		t.Fatalf("incremental update count %d", n)
	}
	if hits, _ := Run(ParseQuery(`"granular mode"`), 10); len(hits) != 1 {
		t.Fatal("appended text not found")
	}
	if n, _ := Update(); n != 0 {
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
