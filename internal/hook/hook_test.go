package hook

import (
	"strings"
	"testing"
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
	t.Setenv(AgyEventEnv, "PostToolUse")
	ev, err := ParseAgy([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if ev.SessionID != "e2419653-70bd" || ev.Cwd != "/w/ws" || ev.Tool != "run_command" ||
		ev.ToolDetail != "Echo cccp-probe string" || ev.Prompt != "Run the shell command" {
		t.Fatalf("%+v", ev)
	}
	t.Setenv(AgyEventEnv, "PreInvocation")
	ev, _ = ParseAgy([]byte(`{"conversationId":"c","invocationNum":0,"initialNumSteps":1}`))
	if !ev.FirstInvocation {
		t.Fatal("invocationNum 0 is the first invocation")
	}
	ev, _ = ParseAgy([]byte(`{"conversationId":"c","invocationNum":1}`))
	if ev.FirstInvocation {
		t.Fatal("invocationNum 1 is not the first invocation")
	}
	t.Setenv(AgyEventEnv, "")
	if _, err := ParseAgy([]byte(raw)); err == nil {
		t.Fatal("missing event name must be an error")
	}
}

func TestClipKeepsUTF8(t *testing.T) {
	s := clip(strings.Repeat("é", 10), 5)
	if s != "éé…" {
		t.Fatalf("%q", s)
	}
}
