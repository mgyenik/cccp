package model

import (
	"strings"
	"time"
)

// TTL buckets of the prompt cache.
const (
	TTL5m = 5 * time.Minute
	TTL1h = time.Hour
)

// ClaudeMeta is what cccp knows from a Claude Code transcript.
type ClaudeMeta struct {
	SessionID string
	AITitle   string
	// LastPrompt is the last prompt the user typed (from "last-prompt"
	// entries, falling back to the last user text message).
	LastPrompt   string
	LastPromptAt time.Time

	LastAssistantAt   time.Time // last main-thread assistant message
	LastAssistantText string
	LastStopReason    string
	Model             string

	// LastRequestAt is the last main-thread API request sent.
	LastRequestAt time.Time
	// TTL is the cache bucket of the most recent main-thread request that
	// wrote to the cache; 0 if unknown.
	TTL time.Duration
	// ContextTokens is input + cache read + cache creation of the last
	// main-thread response.
	ContextTokens int

	// PendingTool is true when the last main-thread assistant message asked
	// for a tool and no tool result has followed yet.
	PendingTool     bool
	PendingToolName string
	// Interrupted is true when the last main-thread user entry is an
	// interrupt marker.
	Interrupted bool
	// LastUserAt is the time of the last main-thread user entry (prompt or
	// tool result).
	LastUserAt time.Time
	// LastTurnAuto: the last main-thread turn was started by Claude Code
	// itself (a background task's or a subagent's report), not by the user.
	// LastTurnAt is when that turn started (zero: none in view).
	LastTurnAuto bool
	LastTurnAt   time.Time
	// RecentPrompts are the user's last typed prompts, oldest first (at most
	// RecentPromptsKept, each clipped): what the conversation is about now.
	RecentPrompts []Prompt
}

// Prompt is one prompt the user typed.
type Prompt struct {
	Text string
	At   time.Time
}

// RecentPromptsKept is how many recent prompts transcripts keep, and
// PromptClip how much of each.
const (
	RecentPromptsKept = 8
	PromptClip        = 400
)

// AddPrompt appends a prompt to recent (oldest first), keeping the last
// RecentPromptsKept, each clipped to PromptClip bytes (not splitting a
// UTF-8 sequence).
func AddPrompt(recent []Prompt, text string, at time.Time) []Prompt {
	text = strings.TrimSpace(text)
	if text == "" {
		return recent
	}
	if len(text) > PromptClip {
		cut := PromptClip
		for cut > 0 && text[cut]&0xC0 == 0x80 {
			cut--
		}
		text = text[:cut] + "…"
	}
	recent = append(recent, Prompt{Text: text, At: at})
	if n := len(recent) - RecentPromptsKept; n > 0 {
		recent = append([]Prompt(nil), recent[n:]...)
	}
	return recent
}

// AutoPrompt reports whether a prompt is one Claude Code sends on its own
// when background work reports: a task notification, or a message from
// another session (a subagent's hand-back). The transcript's turnOrigin
// says so authoritatively; a hook payload has only the text.
func AutoPrompt(p string) bool {
	p = strings.TrimLeft(p, " \t\n")
	return strings.HasPrefix(p, "<task-notification>") || strings.HasPrefix(p, "Another Claude session sent a message:")
}

// LastActivity is when the cache was last refreshed: the later of the last
// request sent and the last response received.
func (m ClaudeMeta) LastActivity() time.Time {
	if m.LastRequestAt.After(m.LastAssistantAt) {
		return m.LastRequestAt
	}
	return m.LastAssistantAt
}

// AgyMeta is what cccp knows from an agy transcript
// (brain/<id>/.system_generated/logs/transcript_full.jsonl).
type AgyMeta struct {
	LastStepIndex  int // step_index of the last step
	LastStepAt     time.Time
	LastStepType   string
	LastStepStatus string
	LastPrompt     string
	LastPromptAt   time.Time
	LastMessage    string
	LastMessageAt  time.Time
	// PendingTools is true when the last planner response requested tools
	// and no later step exists.
	PendingTools  bool
	ContextTokens int
	// LastModelCallAt is the last model call (a planner response, with or
	// without text): what Gemini's prompt cache ages from.
	LastModelCallAt time.Time
	// RecentPrompts are the user's last prompts, oldest first (see
	// ClaudeMeta.RecentPrompts).
	RecentPrompts []Prompt
}
