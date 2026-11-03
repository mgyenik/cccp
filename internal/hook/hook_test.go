package hook

import (
	"strings"
	"testing"
	"time"

	"cccp/internal/model"
)

func TestParseClaudePermissionRequest(t *testing.T) {
	// Shape from the Claude Code hooks reference.
	ev, err := ParseClaude([]byte(`{
	  "session_id": "abc123", "transcript_path": "/t.jsonl", "cwd": "/w",
	  "permission_mode": "default", "hook_event_name": "PermissionRequest",
	  "tool_name": "Bash",
	  "tool_input": {"command": "rm -rf node_modules", "description": "Remove node_modules directory"},
	  "permission_suggestions": [{"type": "addRules"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Name != "PermissionRequest" || ev.SessionID != "abc123" || ev.Tool != "Bash" ||
		ev.ToolDetail != "rm -rf node_modules" || ev.Question != "Allow Bash: rm -rf node_modules" {
		t.Fatalf("%+v", ev)
	}
}

func TestParseClaudeStopFailureAndAsk(t *testing.T) {
	ev, err := ParseClaude([]byte(`{"session_id":"s","hook_event_name":"StopFailure","error":"rate_limit","error_details":"429 Too Many Requests","last_assistant_message":"API Error: Rate limit reached"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Error != "rate_limit 429 Too Many Requests" || ev.LastMessage != "API Error: Rate limit reached" {
		t.Fatalf("%+v", ev)
	}
	ev, err = ParseClaude([]byte(`{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Postgres or SQLite?"},{"question":"Migrate now?"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Question != "Postgres or SQLite? / Migrate now?" {
		t.Fatalf("%q", ev.Question)
	}
}

func TestParseClaudeSubagentAndPrompt(t *testing.T) {
	ev, _ := ParseClaude([]byte(`{"session_id":"s","hook_event_name":"Stop","agent_id":"a1","last_assistant_message":"done"}`))
	if !ev.SubAgent {
		t.Fatal("agent_id should mark a subagent event")
	}
	ev, _ = ParseClaude([]byte(`{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"look at this\n<pasted_content id=\"1\">\nhuge log\n</pasted_content id=\"1\">\nthanks"}`))
	if ev.Prompt != "look at this\n[pasted]\nthanks" {
		t.Fatalf("%q", ev.Prompt)
	}
}

func TestToolDetail(t *testing.T) {
	cases := []struct{ tool, in, want string }{
		{"Edit", `{"file_path":"/a/b.go","old_string":"x"}`, "/a/b.go"},
		{"Grep", `{"pattern":"TODO","path":"src"}`, "TODO  in src"},
		{"Agent", `{"description":"find callers","subagent_type":"Explore"}`, "Explore: find callers"},
		{"mcp__srv__do", `{"zeta":"z","alpha":"a"}`, "alpha=a"},
	}
	for _, c := range cases {
		if got := ToolDetail(c.tool, []byte(c.in)); got != c.want {
			t.Errorf("%s: got %q want %q", c.tool, got, c.want)
		}
	}
}

func TestParseAgy(t *testing.T) {
	// Payload captured from agy 1.4.8 (see docs/agy-hooks.md).
	raw := `{"artifactDirectoryPath":"/b/e24","conversationId":"e2419653-70bd","error":"","lastUserInput":"<USER_REQUEST>\nRun the shell command\n</USER_REQUEST>","modelName":"gemini-4-argon-high","stepIdx":2,"toolCall":{"args":{"CommandLine":"echo cccp-probe","WaitMsBeforeAsync":5000,"toolSummary":"Echo cccp-probe string"},"name":"run_command"},"transcriptPath":"/b/t.jsonl","workspacePaths":["/w/ws"]}`
	ev, err := ParseAgy([]byte(raw), "PostToolUse")
	if err != nil {
		t.Fatal(err)
	}
	if ev.SessionID != "e2419653-70bd" || ev.Cwd != "/w/ws" || ev.Tool != "run_command" ||
		ev.ToolDetail != "Echo cccp-probe string" || ev.Prompt != "Run the shell command" {
		t.Fatalf("%+v", ev)
	}
	ev, _ = ParseAgy([]byte(`{"conversationId":"c","invocationNum":0,"initialNumSteps":1}`), "PreInvocation")
	if !ev.FirstInvocation {
		t.Fatal("invocationNum 0 is the first invocation")
	}
	ev, _ = ParseAgy([]byte(`{"conversationId":"c","invocationNum":1}`), "PreInvocation")
	if ev.FirstInvocation {
		t.Fatal("invocationNum 1 is not the first invocation")
	}
	if _, err := ParseAgy([]byte(raw), ""); err == nil {
		t.Fatal("missing event name must be an error")
	}
}

func TestClipKeepsUTF8(t *testing.T) {
	s := clip(strings.Repeat("é", 10), 5)
	if s != "éé…" {
		t.Fatalf("%q", s)
	}
}

// --- Handle, with fake dependencies --------------------------------------------

type memStore map[string]model.HookState

func (m memStore) Update(a model.Agent, sid string, fn func(model.HookState) model.HookState) error {
	m[string(a)+":"+sid] = fn(m[string(a)+":"+sid])
	return nil
}

func deps(env map[string]string, agent *Agent, st memStore) Deps {
	return Deps{
		Getenv: func(k string) string { return env[k] },
		FindAgent: func(model.Agent) (Agent, bool) {
			if agent == nil {
				return Agent{}, false
			}
			return *agent, true
		},
		Store: st,
	}
}

var hookNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func TestHandleRecordsEvent(t *testing.T) {
	st := memStore{}
	d := deps(map[string]string{"TMUX_PANE": "%7"}, &Agent{PID: 42, Start: 9, Cmdline: []string{"claude"}}, st)
	err := Handle(d, model.Claude, strings.NewReader(`{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":"hi","cwd":"/w"}`), hookNow)
	if err != nil {
		t.Fatal(err)
	}
	s := st["claude:s"]
	if s.Activity != model.Working || s.Pane != "%7" || s.PID != 42 || s.PIDStart != 9 || s.LastPrompt != "hi" ||
		s.Cwd != "/w" || !s.ActivityAt.Equal(hookNow) || s.PrintMode {
		t.Fatalf("%+v", s)
	}
}

func TestHandlePrintModeAndMissingAgent(t *testing.T) {
	st := memStore{}
	d := deps(nil, &Agent{PID: 42, Cmdline: []string{"claude", "-p", "x"}}, st)
	Handle(d, model.Claude, strings.NewReader(`{"session_id":"s","hook_event_name":"Stop"}`), hookNow)
	if !st["claude:s"].PrintMode {
		t.Fatal("print mode not recorded")
	}
	d = deps(nil, nil, st)
	Handle(d, model.Claude, strings.NewReader(`{"session_id":"t","hook_event_name":"Stop"}`), hookNow)
	if s := st["claude:t"]; s.PID != 0 || s.Activity != model.Idle {
		t.Fatalf("%+v", s)
	}
}

func TestHandleInternalIsNoOp(t *testing.T) {
	st := memStore{}
	d := deps(map[string]string{"CCCP_INTERNAL": "1"}, nil, st)
	if err := Handle(d, model.Claude, strings.NewReader(`{"session_id":"s","hook_event_name":"Stop"}`), hookNow); err != nil || len(st) != 0 {
		t.Fatalf("%v %v", err, st)
	}
}

func TestHandleAgyAndErrors(t *testing.T) {
	st := memStore{}
	d := deps(map[string]string{AgyEventEnv: "Stop"}, nil, st)
	if err := Handle(d, model.Agy, strings.NewReader(`{"conversationId":"c","terminationReason":"NO_TOOL_CALL"}`), hookNow); err != nil {
		t.Fatal(err)
	}
	if s := st["agy:c"]; s.Activity != model.Idle || !s.TurnEndedAt.Equal(hookNow) {
		t.Fatalf("%+v", s)
	}
	for name, in := range map[string]string{
		"no session id": `{"hook_event_name":"Stop"}`,
		"bad json":      `{`,
	} {
		if err := Handle(d, model.Claude, strings.NewReader(in), hookNow); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if err := Handle(d, "other", strings.NewReader(`{}`), hookNow); err == nil {
		t.Error("unknown agent accepted")
	}
}

func TestHandleRecordsTheTmuxServerWithThePane(t *testing.T) {
	st := memStore{}
	env := map[string]string{"TMUX_PANE": "%2", "TMUX": "/tmp/tmux-1000/work,999,0"}
	Handle(deps(env, nil, st), model.Claude, strings.NewReader(`{"session_id":"s","hook_event_name":"Stop"}`), hookNow)
	if s := st["claude:s"]; s.Pane != "%2" || s.TmuxSocket != "/tmp/tmux-1000/work" {
		t.Fatalf("pane %q server %q", s.Pane, s.TmuxSocket)
	}
	// Outside tmux: neither.
	st = memStore{}
	Handle(deps(map[string]string{"TMUX": "/stale,1,0"}, nil, st), model.Claude, strings.NewReader(`{"session_id":"s","hook_event_name":"Stop"}`), hookNow)
	if s := st["claude:s"]; s.Pane != "" || s.TmuxSocket != "" {
		t.Fatalf("pane %q server %q", s.Pane, s.TmuxSocket)
	}
}
