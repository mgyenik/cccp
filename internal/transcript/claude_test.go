package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cccp/internal/model"
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

// entry is a main-thread user entry with Claude Code's turn fields.
func entry(sec int, text, extra string) string {
	return fmt.Sprintf(`{"type":"user","isSidechain":false,"sessionId":"S","timestamp":%q,%s"message":{"role":"user","content":%q}}`, ts(sec), extra, text)
}

// Who started the last turn: the user, or Claude Code on a background
// task's or subagent's report (as the real entries record it, 2.1.318).
func TestLastTurnOrigin(t *testing.T) {
	p := filepath.Join(t.TempDir(), "S.jsonl")
	r := NewReader()
	turn := func(lines ...string) model.ClaudeMeta {
		t.Helper()
		write(t, p, lines...)
		m, err := r.Claude(p)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := turn(entry(1, "check on the agents", `"promptSource":"typed","turnOrigin":"human",`), asst(2, "end_turn", `{"type":"text","text":"ok"}`, 0, 0))
	if m.LastTurnAuto || !m.LastTurnAt.Equal(time.Date(2026, 11, 2, 12, 0, 1, 0, time.UTC)) {
		t.Fatalf("typed: %+v", m)
	}
	m = turn(entry(3, "<task-notification> <task-id>b1</task-id> <status>completed</status>", `"promptSource":"system","turnOrigin":"task_notification",`), asst(4, "end_turn", `{"type":"text","text":"still waiting"}`, 0, 0))
	if !m.LastTurnAuto || m.LastPrompt != "check on the agents" {
		t.Fatalf("task notification: %+v", m)
	}
	// A subagent's hand-back is a meta entry, but it starts a turn.
	m = turn(entry(5, "Another Claude session sent a message:\n<agent-message from=\"a1\">", `"isMeta":true,"promptSource":"system","turnOrigin":"peer",`))
	if !m.LastTurnAuto || m.LastTurnAt.Second() != 5 {
		t.Fatalf("peer: %+v", m)
	}
	// Tool results and system reminders start no turn.
	m = turn(toolResult(6), entry(7, "<system-reminder>x</system-reminder>", `"isMeta":true,`))
	if !m.LastTurnAuto || m.LastTurnAt.Second() != 5 {
		t.Fatalf("no new turn: %+v", m)
	}
	m = turn(entry(8, "go ahead", `"promptSource":"suggestion_accepted","turnOrigin":"human",`))
	if m.LastTurnAuto {
		t.Fatalf("an accepted suggestion is the user's: %+v", m)
	}
	// A tool result never starts a turn, even with text beside it.
	mixed := fmt.Sprintf(`{"type":"user","isSidechain":false,"sessionId":"S","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"},{"type":"text","text":"note"}]}}`, ts(8))
	if m = turn(entry(8, "<task-notification>", `"turnOrigin":"task_notification",`), mixed); !m.LastTurnAuto || m.LastTurnAt.Second() != 8 {
		t.Fatalf("tool result with text: %+v", m)
	}
	// promptSource alone (no turnOrigin) decides too.
	if m = turn(entry(8, "typed text", `"promptSource":"typed",`)); m.LastTurnAuto {
		t.Fatalf("typed: %+v", m)
	}
	if m = turn(entry(8, "a scheduled prompt", `"promptSource":"system",`)); !m.LastTurnAuto {
		t.Fatalf("system: %+v", m)
	}
	// A meta entry without the fields (a caveat) starts no turn.
	if m = turn(entry(9, "Caveat: the messages below were generated", `"isMeta":true,`)); !m.LastTurnAuto || m.LastTurnAt.Second() != 8 {
		t.Fatalf("meta caveat: %+v", m)
	}
	// Older entries without the fields: a task notification by its text,
	// a plain prompt as the user's.
	if m = turn(user(9, "plain"), user(9, "<task-notification> <task-id>b2</task-id>")); !m.LastTurnAuto {
		t.Fatalf("old notification: %+v", m)
	}
	if m = turn(user(10, "next")); m.LastTurnAuto || m.LastTurnAt.Second() != 10 {
		t.Fatalf("old prompt: %+v", m)
	}
}

// The working title reads the user's own recent prompts: typed ones only,
// the last few, clipped.
func TestRecentPrompts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "S.jsonl")
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, entry(i, fmt.Sprintf("prompt %d", i), `"promptSource":"typed","turnOrigin":"human",`))
	}
	lines = append(lines,
		entry(11, "<task-notification> done", `"promptSource":"system","turnOrigin":"task_notification",`),
		entry(12, "Another Claude session sent a message:\nhi", `"isMeta":true,"promptSource":"system","turnOrigin":"peer",`),
		entry(13, "<command-name>/model</command-name>", `"promptSource":"typed","turnOrigin":"human",`),
		entry(14, strings.Repeat("é", 400), `"promptSource":"typed","turnOrigin":"human",`))
	write(t, p, lines...)
	m, err := NewReader().Claude(p)
	if err != nil {
		t.Fatal(err)
	}
	rp := m.RecentPrompts
	if len(rp) != model.RecentPromptsKept || rp[0].Text != "prompt 4" || rp[6].Text != "prompt 10" || rp[6].At.Second() != 10 {
		t.Fatalf("%+v", rp)
	}
	if last := rp[7].Text; !strings.HasSuffix(last, "…") || len(last) > model.PromptClip+len("…") || strings.ContainsRune(last, '�') {
		t.Fatalf("clipped: %q", last)
	}
}
