// Package model defines the types shared by the hook, engine, daemon and UI.
package model

import "time"

// Agent identifies the coding agent a session belongs to.
type Agent string

const (
	Claude Agent = "claude"
	Agy    Agent = "agy"
)

// Activity is what a session is doing right now.
type Activity string

const (
	Working       Activity = "working"
	NeedsApproval Activity = "needs_approval"
	NeedsInput    Activity = "needs_input"
	Idle          Activity = "idle"
	Errored       Activity = "error"
	Ended         Activity = "ended"
	Unknown       Activity = "unknown"
)

// NeedsYou reports whether the activity is blocked on the user.
func (a Activity) NeedsYou() bool {
	return a == NeedsApproval || a == NeedsInput || a == Errored
}

// HookStateVersion is bumped when HookState changes incompatibly.
const HookStateVersion = 1

// HookState is the per-session state reduced from hook events. It is written
// by `cccp hook` (under a lock, via atomic rename) and only read by everything
// else.
type HookState struct {
	Version   int    `json:"version"`
	Agent     Agent  `json:"agent"`
	SessionID string `json:"session_id"`

	PID      int    `json:"pid,omitempty"`
	PIDStart uint64 `json:"pid_start,omitempty"` // /proc/<pid>/stat field 22
	Pane     string `json:"pane,omitempty"`      // tmux pane id, e.g. "%21"

	Cwd            string `json:"cwd,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	Model          string `json:"model,omitempty"`
	CustomTitle    string `json:"custom_title,omitempty"`

	Activity   Activity  `json:"activity"`
	ActivityAt time.Time `json:"activity_at"` // time of the event that set Activity

	LastEvent   string    `json:"last_event"`
	LastEventAt time.Time `json:"last_event_at"`

	Tool       string `json:"tool,omitempty"`        // current (claude) or last completed (agy) tool
	ToolDetail string `json:"tool_detail,omitempty"` // one-line preview of the tool input
	Question   string `json:"question,omitempty"`    // what the session is waiting on, when it waits

	LastPrompt    string    `json:"last_prompt,omitempty"`
	LastPromptAt  time.Time `json:"last_prompt_at,omitempty"`
	LastMessage   string    `json:"last_message,omitempty"`
	LastMessageAt time.Time `json:"last_message_at,omitempty"`

	// TurnEndedAt is the time the last turn finished (normally or with an
	// error). It is the source of "your turn" attention items.
	TurnEndedAt time.Time `json:"turn_ended_at,omitempty"`

	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	EndReason string    `json:"end_reason,omitempty"`

	ToolCalls   int `json:"tool_calls,omitempty"`
	Compactions int `json:"compactions,omitempty"`
}

// Event is a normalised hook event.
type Event struct {
	Agent Agent
	Name  string // the agent's hook event name, e.g. "PreToolUse"
	Time  time.Time

	SessionID      string
	Cwd            string
	TranscriptPath string
	PermissionMode string
	Model          string
	CustomTitle    string

	PID      int
	PIDStart uint64
	Pane     string

	// Event-specific fields; which are set depends on Name.
	Source           string // SessionStart source; PreCompact trigger
	Prompt           string
	Tool             string
	ToolDetail       string
	Question         string
	NotificationType string
	Message          string
	LastMessage      string
	Error            string
	EndReason        string
	SubAgent         bool // event fired inside a subagent
	TerminationCause string
	FirstInvocation  bool
}
