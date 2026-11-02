package reduce

import (
	"testing"
	"time"

	"cccp/internal/model"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

func cl(name string, sec int, f ...func(*model.Event)) model.Event {
	ev := model.Event{Agent: model.Claude, Name: name, SessionID: "s1", Time: at(sec)}
	for _, fn := range f {
		fn(&ev)
	}
	return ev
}

func run(evs ...model.Event) model.HookState {
	var s model.HookState
	for _, ev := range evs {
		s = Apply(s, ev)
	}
	return s
}

func TestClaudeTurn(t *testing.T) {
	s := run(
		cl("SessionStart", 0, func(e *model.Event) { e.Source = "startup"; e.PID = 42; e.Pane = "%3" }),
		cl("UserPromptSubmit", 1, func(e *model.Event) { e.Prompt = "fix the bug" }),
		cl("PreToolUse", 2, func(e *model.Event) { e.Tool = "Bash"; e.ToolDetail = "go test ./..." }),
	)
	if s.Activity != model.Working || s.Tool != "Bash" || s.ToolDetail != "go test ./..." {
		t.Fatalf("mid-turn: %+v", s)
	}
	if s.PID != 42 || s.Pane != "%3" || s.LastPrompt != "fix the bug" || !s.StartedAt.Equal(at(0)) {
		t.Fatalf("metadata: %+v", s)
	}
	s = Apply(s, cl("PostToolUse", 5))
	s = Apply(s, cl("Stop", 9, func(e *model.Event) { e.LastMessage = "Fixed. Want me to commit?" }))
	if s.Activity != model.Idle || !s.TurnEndedAt.Equal(at(9)) || s.LastMessage != "Fixed. Want me to commit?" {
		t.Fatalf("after stop: %+v", s)
	}
	if s.Tool != "" || s.Question != "" {
		t.Fatalf("stop must clear tool/question: %+v", s)
	}
	if s.ToolCalls != 1 {
		t.Fatalf("tool calls = %d", s.ToolCalls)
	}
}

func TestPermissionRequestThenApproved(t *testing.T) {
	s := run(
		cl("UserPromptSubmit", 1),
		cl("PreToolUse", 2, func(e *model.Event) { e.Tool = "Bash"; e.ToolDetail = "rm -rf build" }),
		cl("PermissionRequest", 2, func(e *model.Event) {
			e.Tool, e.ToolDetail, e.Question = "Bash", "rm -rf build", "Allow Bash: rm -rf build"
		}),
	)
	if s.Activity != model.NeedsApproval || s.Question != "Allow Bash: rm -rf build" {
		t.Fatalf("want needs_approval: %+v", s)
	}
	// The ~6 s notification arrives later and must not replace the detail.
	s = Apply(s, cl("Notification", 8, func(e *model.Event) {
		e.NotificationType, e.Message = "permission_prompt", "Claude needs your permission to use Bash"
	}))
	if s.Question != "Allow Bash: rm -rf build" {
		t.Fatalf("notification replaced detail: %q", s.Question)
	}
	s = Apply(s, cl("PostToolUse", 20))
	if s.Activity != model.Working || s.Question != "" {
		t.Fatalf("after approval: %+v", s)
	}
}

func TestOutOfOrderEventDoesNotChangeActivity(t *testing.T) {
	s := run(cl("UserPromptSubmit", 1), cl("Stop", 10))
	// A Notification taken at t=5 but delivered after Stop (async hook).
	s = Apply(s, cl("Notification", 5, func(e *model.Event) { e.NotificationType = "permission_prompt" }))
	if s.Activity != model.Idle {
		t.Fatalf("stale event changed activity: %+v", s)
	}
	if s.LastEvent != "Stop" {
		t.Fatalf("stale event became last event: %q", s.LastEvent)
	}
}

func TestIdlePromptCorrectsMissedStop(t *testing.T) {
	// The user interrupted (no Stop fires); idle_prompt arrives later.
	s := run(cl("UserPromptSubmit", 1), cl("PreToolUse", 2, func(e *model.Event) { e.Tool = "Bash" }))
	s = Apply(s, cl("Notification", 70, func(e *model.Event) { e.NotificationType = "idle_prompt" }))
	if s.Activity != model.Idle {
		t.Fatalf("want idle: %+v", s)
	}
	if !s.TurnEndedAt.IsZero() {
		t.Fatalf("an interrupt is not a completion: %v", s.TurnEndedAt)
	}
}

func TestAskUserQuestion(t *testing.T) {
	s := run(cl("PreToolUse", 2, func(e *model.Event) {
		e.Tool, e.Question = "AskUserQuestion", "Which database?"
	}))
	if s.Activity != model.NeedsInput || s.Question != "Which database?" {
		t.Fatalf("%+v", s)
	}
}

func TestStopFailure(t *testing.T) {
	s := run(cl("UserPromptSubmit", 1), cl("StopFailure", 3, func(e *model.Event) { e.Error = "rate_limit 429" }))
	if s.Activity != model.Errored || s.Error != "rate_limit 429" || !s.TurnEndedAt.Equal(at(3)) {
		t.Fatalf("%+v", s)
	}
	s = Apply(s, cl("UserPromptSubmit", 30))
	if s.Error != "" || s.Activity != model.Working {
		t.Fatalf("new prompt should clear the error: %+v", s)
	}
}

func TestSubagentStopIgnored(t *testing.T) {
	s := run(cl("UserPromptSubmit", 1), cl("Stop", 4, func(e *model.Event) { e.SubAgent = true }))
	if s.Activity != model.Working {
		t.Fatalf("subagent stop ended the turn: %+v", s)
	}
}

func TestCompaction(t *testing.T) {
	s := run(cl("Stop", 1), cl("PreCompact", 2, func(e *model.Event) { e.Source = "manual" }))
	if s.Activity != model.Working || s.Tool != "compact" {
		t.Fatalf("%+v", s)
	}
	s = Apply(s, cl("PostCompact", 9, func(e *model.Event) { e.Source = "manual" }))
	if s.Activity != model.Idle || !s.TurnEndedAt.Equal(at(1)) {
		t.Fatalf("manual compact should end idle without a new completion: %+v", s)
	}
	// SessionStart with source compact keeps the activity.
	s = Apply(s, cl("PreCompact", 20, func(e *model.Event) { e.Source = "auto" }))
	s = Apply(s, cl("SessionStart", 21, func(e *model.Event) { e.Source = "compact" }))
	if s.Activity != model.Working {
		t.Fatalf("compact SessionStart reset activity: %+v", s)
	}
}

func TestSessionEndAndRestart(t *testing.T) {
	s := run(cl("Stop", 1), cl("SessionEnd", 2, func(e *model.Event) { e.EndReason = "prompt_input_exit" }))
	if s.Activity != model.Ended || s.EndReason != "prompt_input_exit" || !s.EndedAt.Equal(at(2)) {
		t.Fatalf("%+v", s)
	}
	s = Apply(s, cl("SessionStart", 100, func(e *model.Event) { e.Source = "resume"; e.PID = 77 }))
	if s.Activity != model.Idle || !s.EndedAt.IsZero() || s.PID != 77 {
		t.Fatalf("resume: %+v", s)
	}
}

func ag(name string, sec int, f ...func(*model.Event)) model.Event {
	ev := model.Event{Agent: model.Agy, Name: name, SessionID: "c1", Time: at(sec)}
	for _, fn := range f {
		fn(&ev)
	}
	return ev
}

func TestAgyTurn(t *testing.T) {
	s := run(
		ag("SessionStart", 0, func(e *model.Event) { e.Prompt = "hello" }),
		ag("PreInvocation", 0, func(e *model.Event) { e.FirstInvocation = true; e.Prompt = "hello" }),
		ag("PostToolUse", 3, func(e *model.Event) { e.Tool = "run_command"; e.ToolDetail = "echo hi" }),
		ag("PreInvocation", 4),
	)
	if s.Activity != model.Working || s.Tool != "run_command" || s.LastPrompt != "hello" {
		t.Fatalf("%+v", s)
	}
	s = Apply(s, ag("Stop", 6, func(e *model.Event) { e.TerminationCause = "NO_TOOL_CALL" }))
	if s.Activity != model.Idle || !s.TurnEndedAt.Equal(at(6)) {
		t.Fatalf("%+v", s)
	}
}

func TestAgyCancelIsNotACompletion(t *testing.T) {
	s := run(ag("PreInvocation", 1), ag("Stop", 2, func(e *model.Event) { e.TerminationCause = "USER_CANCELED" }))
	if s.Activity != model.Idle || !s.TurnEndedAt.IsZero() {
		t.Fatalf("%+v", s)
	}
	s = Apply(s, ag("PreInvocation", 3))
	s = Apply(s, ag("Stop", 4, func(e *model.Event) { e.TerminationCause = "ERROR"; e.Error = "quota" }))
	if s.Activity != model.Errored || s.Error != "quota" {
		t.Fatalf("%+v", s)
	}
}
