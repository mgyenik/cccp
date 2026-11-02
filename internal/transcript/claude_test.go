package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ts(sec int) string {
	return time.Date(2026, 11, 2, 12, 0, sec, 0, time.UTC).Format(time.RFC3339Nano)
}

func user(sec int, text string) string {
	return fmt.Sprintf(`{"type":"user","isSidechain":false,"sessionId":"S","timestamp":%q,"message":{"role":"user","content":%q}}`, ts(sec), text)
}

func toolResult(sec int) string {
	return fmt.Sprintf(`{"type":"user","isSidechain":false,"sessionId":"S","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"}]}}`, ts(sec))
}

func asst(sec int, stop string, blocks string, h1, m5 int) string {
	return fmt.Sprintf(`{"type":"assistant","isSidechain":false,"sessionId":"S","timestamp":%q,"message":{"model":"claude-x","stop_reason":%q,"content":[%s],"usage":{"input_tokens":10,"cache_read_input_tokens":1000,"cache_creation_input_tokens":%d,"cache_creation":{"ephemeral_1h_input_tokens":%d,"ephemeral_5m_input_tokens":%d}}}}`, ts(sec), stop, blocks, h1+m5, h1, m5)
}

func write(t *testing.T, p string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
}

func TestClaudeMetaBasics(t *testing.T) {
	p := filepath.Join(t.TempDir(), "S.jsonl")
	write(t, p,
		`{"type":"ai-title","aiTitle":"Refactor parser","sessionId":"S"}`,
		user(1, "please refactor"),
		asst(2, "tool_use", `{"type":"text","text":"Looking."},{"type":"tool_use","name":"Bash","id":"t","input":{}}`, 500, 0),
	)
	r := NewReader()
	m, err := r.Claude(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.AITitle != "Refactor parser" || m.SessionID != "S" || m.TTL != TTL1h || !m.PendingTool || m.PendingToolName != "Bash" {
		t.Fatalf("%+v", m)
	}
	if m.ContextTokens != 10+1000+500 || m.LastPrompt != "please refactor" {
		t.Fatalf("%+v", m)
	}
	// Tool result resolves the pending tool; final answer ends the turn.
	write(t, p, toolResult(3), asst(4, "end_turn", `{"type":"text","text":"Done. Commit?"}`, 0, 0))
	m, _ = r.Claude(p)
	if m.PendingTool || m.LastStopReason != "end_turn" || m.LastAssistantText != "Done. Commit?" {
		t.Fatalf("%+v", m)
	}
	// No cache write on the last request: TTL stays from the earlier one.
	if m.TTL != TTL1h {
		t.Fatalf("ttl %v", m.TTL)
	}
	// Interrupt marker.
	write(t, p, user(5, "[Request interrupted by user]"))
	m, _ = r.Claude(p)
	if !m.Interrupted || m.LastPrompt != "please refactor" {
		t.Fatalf("%+v", m)
	}
}

func TestClaudeMetaPartialLineAndWindows(t *testing.T) {
	old1, old2 := initialWindow, maxWindow
	initialWindow, maxWindow = 512, 1<<20
	defer func() { initialWindow, maxWindow = old1, old2 }()

	p := filepath.Join(t.TempDir(), "S.jsonl")
	// The title is far from the end: the backward scan must grow its window.
	lines := []string{`{"type":"ai-title","aiTitle":"Early title","sessionId":"S"}`, `{"type":"last-prompt","lastPrompt":"the prompt","sessionId":"S"}`}
	lines = append(lines, asst(1, "end_turn", `{"type":"text","text":"x"}`, 0, 77))
	for i := 0; i < 200; i++ {
		lines = append(lines, fmt.Sprintf(`{"type":"attachment","n":%d,"pad":%q}`, i, strings.Repeat("p", 50)))
	}
	write(t, p, lines...)
	// A partially written final line must be ignored until complete.
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprint(f, `{"type":"ai-title","aiTitle":"Half`)
	f.Close()

	r := NewReader()
	m, err := r.Claude(p)
	if err != nil {
		t.Fatal(err)
	}
	if m.AITitle != "Early title" || m.LastPrompt != "the prompt" || m.TTL != TTL5m {
		t.Fatalf("%+v", m)
	}
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprintln(f, ` written","sessionId":"S"}`)
	f.Close()
	m, _ = r.Claude(p)
	if m.AITitle != "Half written" {
		t.Fatalf("completed line not picked up: %+v", m)
	}
}

func TestClaudeSidechainIgnored(t *testing.T) {
	p := filepath.Join(t.TempDir(), "S.jsonl")
	write(t, p,
		user(1, "main prompt"),
		asst(2, "end_turn", `{"type":"text","text":"main answer"}`, 1, 0),
		strings.Replace(asst(3, "tool_use", `{"type":"tool_use","name":"Read"}`, 0, 0), `"isSidechain":false`, `"isSidechain":true`, 1),
	)
	m, _ := NewReader().Claude(p)
	if m.PendingTool || m.LastAssistantText != "main answer" {
		t.Fatalf("sidechain leaked: %+v", m)
	}
}
