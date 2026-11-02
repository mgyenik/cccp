package model

import "time"

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
}
