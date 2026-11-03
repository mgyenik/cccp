// Package reduce applies hook events to a session's HookState. It is pure: no
// I/O, no clock. All decisions about what an event means live here.
package reduce

import (
	"time"

	"cccp/internal/model"
)

var timeZero time.Time

// Apply returns the state after ev. s may be the zero value for a session
// that has no state yet.
//
// Ordering: Claude Code runs some hooks asynchronously (Notification, and the
// standalone events), so events can arrive slightly out of order. An event
// older than the event that set the current activity updates metadata only;
// it never changes the activity.
func Apply(s model.HookState, ev model.Event) model.HookState {
	s.Version = model.HookStateVersion
	if s.Agent == "" {
		s.Agent = ev.Agent
	}
	if s.SessionID == "" {
		s.SessionID = ev.SessionID
	}
	mergeMeta(&s, ev)

	fresh := !ev.Time.Before(s.ActivityAt)
	if ev.Time.After(s.LastEventAt) || s.LastEventAt.IsZero() {
		s.LastEvent = ev.Name
		s.LastEventAt = ev.Time
	}

	switch ev.Agent {
	case model.Claude:
		applyClaude(&s, ev, fresh)
	case model.Agy:
		applyAgy(&s, ev, fresh)
	}
	return s
}

func mergeMeta(s *model.HookState, ev model.Event) {
	if ev.PID != 0 {
		s.PID, s.PIDStart = ev.PID, ev.PIDStart
		s.PrintMode = ev.PrintMode
		s.ConfigDir = ev.ConfigDir
	}
	if ev.Pane != "" {
		// A pane id means nothing without its server: always set together.
		s.Pane, s.TmuxSocket = ev.Pane, ev.TmuxSocket
	}
	if ev.Cwd != "" {
		s.Cwd = ev.Cwd
	}
	if ev.TranscriptPath != "" {
		s.TranscriptPath = ev.TranscriptPath
	}
	if ev.PermissionMode != "" {
		s.PermissionMode = ev.PermissionMode
	}
	if ev.Model != "" {
		s.Model = ev.Model
	}
	if ev.CustomTitle != "" {
		s.CustomTitle = ev.CustomTitle
	}
	if s.StartedAt.IsZero() || (!ev.Time.IsZero() && ev.Time.Before(s.StartedAt)) {
		s.StartedAt = ev.Time
	}
}

func set(s *model.HookState, ev model.Event, fresh bool, a model.Activity) bool {
	if !fresh {
		return false
	}
	s.Activity = a
	s.ActivityAt = ev.Time
	return true
}

func applyClaude(s *model.HookState, ev model.Event, fresh bool) {
	switch ev.Name {
	case "SessionStart":
		// A (re)started session; resumed and cleared sessions start idle.
		// "compact" fires after compaction, which can happen mid-turn, so
		// it does not reset the activity.
		s.EndedAt, s.EndReason = timeZero, ""
		if ev.Source == "compact" {
			return
		}
		if set(s, ev, fresh, model.Idle) {
			s.Question, s.Error = "", ""
		}

	case "UserPromptSubmit":
		if ev.Prompt != "" && !ev.Time.Before(s.LastPromptAt) {
			s.LastPrompt, s.LastPromptAt = ev.Prompt, ev.Time
		}
		if set(s, ev, fresh, model.Working) {
			s.Question, s.Error, s.Tool, s.ToolDetail = "", "", "", ""
		}

	case "PreToolUse":
		s.ToolCalls++
		switch ev.Tool {
		case "AskUserQuestion":
			if set(s, ev, fresh, model.NeedsInput) {
				s.Tool, s.ToolDetail, s.Question = ev.Tool, ev.ToolDetail, ev.Question
			}
		default:
			if set(s, ev, fresh, model.Working) {
				s.Tool, s.ToolDetail, s.Question = ev.Tool, ev.ToolDetail, ""
			}
		}

	case "PermissionRequest":
		if set(s, ev, fresh, model.NeedsApproval) {
			s.Tool, s.ToolDetail = ev.Tool, ev.ToolDetail
			s.Question = ev.Question
		}

	case "PostToolUse", "PostToolUseFailure", "PermissionDenied", "PostToolBatch", "ElicitationResult":
		if set(s, ev, fresh, model.Working) {
			s.Question = ""
		}

	case "Elicitation":
		if set(s, ev, fresh, model.NeedsInput) {
			s.Question = ev.Question
		}

	case "Notification":
		switch ev.NotificationType {
		case "permission_prompt":
			// Fires ~6 s after the prompt appeared; PermissionRequest
			// normally got there first. Keep the richer detail if so.
			if s.Activity != model.NeedsApproval && set(s, ev, fresh, model.NeedsApproval) {
				s.Question = ev.Message
			}
		case "elicitation_dialog", "elicitation_url_dialog":
			if s.Activity != model.NeedsInput && set(s, ev, fresh, model.NeedsInput) {
				s.Question = ev.Message
			}
		case "idle_prompt":
			// Claude finished ~60 s ago and is waiting for a prompt. If we
			// still think it is working, a turn end was missed (an
			// interrupt fires no Stop): correct the activity, but this is
			// not a new completion.
			if s.Activity == model.Working {
				set(s, ev, fresh, model.Idle)
			}
		}

	case "Stop":
		if ev.SubAgent {
			return
		}
		if ev.LastMessage != "" {
			s.LastMessage, s.LastMessageAt = ev.LastMessage, ev.Time
		}
		if set(s, ev, fresh, model.Idle) {
			s.TurnEndedAt = ev.Time
			s.Question, s.Tool, s.ToolDetail = "", "", ""
		}

	case "StopFailure":
		if ev.LastMessage != "" {
			s.LastMessage, s.LastMessageAt = ev.LastMessage, ev.Time
		}
		if set(s, ev, fresh, model.Errored) {
			s.TurnEndedAt = ev.Time
			s.Error = ev.Error
			s.Question = ""
		}

	case "PreCompact":
		s.Compactions++
		if set(s, ev, fresh, model.Working) {
			s.Tool, s.ToolDetail = "compact", ev.Source
		}

	case "PostCompact":
		// A manual /compact is a user action with no Stop afterwards; an
		// automatic one happens inside a turn that continues.
		if ev.Source == "manual" {
			set(s, ev, fresh, model.Idle)
		}

	case "SessionEnd":
		if set(s, ev, fresh, model.Ended) {
			s.EndedAt = ev.Time
			s.EndReason = ev.EndReason
			s.Question = ""
		}
	}
}

func applyAgy(s *model.HookState, ev model.Event, fresh bool) {
	switch ev.Name {
	case "SessionStart":
		// agy fires SessionStart lazily, just before the first model call.
		s.EndedAt, s.EndReason = timeZero, ""
		if ev.Prompt != "" {
			s.LastPrompt, s.LastPromptAt = ev.Prompt, ev.Time
		}
		set(s, ev, fresh, model.Working)

	case "PreInvocation":
		if ev.FirstInvocation && ev.Prompt != "" && !ev.Time.Before(s.LastPromptAt) {
			s.LastPrompt, s.LastPromptAt = ev.Prompt, ev.Time
		}
		if set(s, ev, fresh, model.Working) {
			s.Question, s.Error = "", ""
		}

	case "PostToolUse":
		s.ToolCalls++
		s.Tool, s.ToolDetail = ev.Tool, ev.ToolDetail
		set(s, ev, fresh, model.Working)

	case "Stop":
		switch ev.TerminationCause {
		case "ERROR":
			if set(s, ev, fresh, model.Errored) {
				s.TurnEndedAt = ev.Time
				s.Error = ev.Error
			}
		case "USER_CANCELED":
			// The user was there to cancel it: not a completion to flag.
			set(s, ev, fresh, model.Idle)
		default:
			if set(s, ev, fresh, model.Idle) {
				s.TurnEndedAt = ev.Time
			}
		}
	}
}
