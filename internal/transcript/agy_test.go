package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgyMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	lines := []string{
		`{"type":"USER_INPUT","created_at":"2026-11-04T10:00:00Z","content":"<USER_REQUEST>build it</USER_REQUEST>"}`,
		`{"type":"PLANNER_RESPONSE","created_at":"2026-11-04T10:00:05Z","content":"On it.","input_tokens":100,"cache_read_tokens":5000}`,
		`{"type":"GENERIC","created_at":"2026-11-04T10:00:06Z","content":"tool output"}`,
		// A model call that only calls tools: no text, but a model call.
		`{"step_index":3,"type":"PLANNER_RESPONSE","created_at":"2026-11-04T10:00:09Z","content":"","tool_calls":[{"name":"x"}],"input_tokens":300,"cache_read_tokens":6000}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewReader().Agy(path)
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	if !m.LastModelCallAt.Equal(at("2026-11-04T10:00:09Z")) {
		t.Fatalf("last model call %v (a tool-only planner response counts)", m.LastModelCallAt)
	}
	if m.LastMessage != "On it." || !m.LastMessageAt.Equal(at("2026-11-04T10:00:05Z")) {
		t.Fatalf("last message %q at %v (the last one with text)", m.LastMessage, m.LastMessageAt)
	}
	if len(m.RecentPrompts) != 1 || m.RecentPrompts[0].Text != "build it" || !m.RecentPrompts[0].At.Equal(at("2026-11-04T10:00:00Z")) {
		t.Fatalf("recent prompts %+v", m.RecentPrompts)
	}
	if m.LastPrompt != "build it" || !m.PendingTools || m.ContextTokens != 6300 || m.LastStepIndex != 3 {
		t.Fatalf("%+v", m)
	}
}
