// Package core is the functional core of cccp. Given a plain-data
// Observation of every source (hook state files, Claude's session registry,
// tmux, agent processes, transcripts) and the remembered Memory, Compute
// derives the Snapshot of all sessions. Nothing in this package performs
// I/O or reads the clock: every input is a parameter, so every rule is
// testable with literal values.
package core

import (
	"time"

	"cccp/internal/model"
)

// Group is a display group, in priority order.
type Group int

const (
	GroupNeedsYou Group = iota
	GroupYourTurn
	GroupWorking
	GroupIdle
	GroupCold
	GroupUnknown
	GroupClosed
)

// Groups lists all groups in display order.
var Groups = []Group{GroupNeedsYou, GroupYourTurn, GroupWorking, GroupIdle, GroupCold, GroupUnknown, GroupClosed}

func (g Group) String() string {
	switch g {
	case GroupNeedsYou:
		return "NEEDS YOU"
	case GroupYourTurn:
		return "YOUR TURN"
	case GroupWorking:
		return "WORKING"
	case GroupIdle:
		return "IDLE"
	case GroupCold:
		return "COLD"
	case GroupUnknown:
		return "UNKNOWN"
	case GroupClosed:
		return "CLOSED"
	}
	return "?"
}

// Tracking says which sources observe a session.
type Tracking string

const (
	TrackHooks         Tracking = "hooks"
	TrackRegistry      Tracking = "registry"
	TrackHooksRegistry Tracking = "hooks+registry"
	// TrackUnmanaged: found only via tmux and /proc; state is inferred.
	TrackUnmanaged Tracking = "unmanaged"
	// TrackCatalog: known only from its transcript (no process).
	TrackCatalog Tracking = "catalog"
)

// Cache describes the prompt cache of a Claude session.
type Cache struct {
	Known        bool          `json:"known"`         // a last-activity time is known
	TTL          time.Duration `json:"ttl"`           // bucket
	TTLAssumed   bool          `json:"ttl_assumed"`   // TTL is the configured default
	LastActivity time.Time     `json:"last_activity"` // last request or response
	ExpiresAt    time.Time     `json:"expires_at"`
}

// Warm reports whether the cache is warm at t.
func (c Cache) Warm(t time.Time) bool { return c.Known && t.Before(c.ExpiresAt) }

// Session is one agent session as presented to users.
type Session struct {
	Key       string      `json:"key"` // "agent:session-id", or "agent:pane:%N" when the id is unknown
	Agent     model.Agent `json:"agent"`
	SessionID string      `json:"session_id,omitempty"`
	Tracking  Tracking    `json:"tracking"`
	// Inferred is true when the activity comes from a heuristic rather than
	// an observation.
	Inferred bool `json:"inferred,omitempty"`

	Activity      model.Activity `json:"activity"`
	ActivitySince time.Time      `json:"activity_since,omitempty"`
	Tool          string         `json:"tool,omitempty"`
	ToolDetail    string         `json:"tool_detail,omitempty"`
	Question      string         `json:"question,omitempty"`
	Error         string         `json:"error,omitempty"`

	Title       string `json:"title"`
	TitleSource string `json:"title_source,omitempty"`
	Alias       string `json:"alias,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
	// Profile is the Claude profile's name and ConfigDir its directory
	// (Claude sessions only).
	Profile   string `json:"profile,omitempty"`
	ConfigDir string `json:"config_dir,omitempty"`
	// DefaultProfile: ConfigDir is the default profile's, selected by
	// leaving CLAUDE_CONFIG_DIR unset (see model.Profile.Environ).
	DefaultProfile bool   `json:"default_profile,omitempty"`
	Model          string `json:"model,omitempty"`

	Pane     string `json:"pane,omitempty"`
	WindowID string `json:"window_id,omitempty"`
	Location string `json:"location,omitempty"`
	TmuxSess string `json:"tmux_session,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Kind     string `json:"kind,omitempty"` // interactive / background
	Alive    bool   `json:"alive"`
	Visible  bool   `json:"visible,omitempty"`

	FirstPrompt  string    `json:"first_prompt,omitempty"`
	LastPrompt   string    `json:"last_prompt,omitempty"`
	LastPromptAt time.Time `json:"last_prompt_at,omitempty"`
	LastMessage  string    `json:"last_message,omitempty"`
	Branch       string    `json:"branch,omitempty"`
	Turns        int       `json:"turns,omitempty"` // prompts typed (from the catalog)

	// CompletedAt is the end of the last turn; Unseen means it has not been
	// looked at since.
	CompletedAt time.Time `json:"completed_at,omitempty"`
	Unseen      bool      `json:"unseen,omitempty"`

	Cache         Cache `json:"cache"`
	ContextTokens int   `json:"context_tokens,omitempty"`

	TranscriptPath string    `json:"transcript_path,omitempty"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	EndedAt        time.Time `json:"ended_at,omitempty"`
	LastEventAt    time.Time `json:"last_event_at,omitempty"`

	Group Group    `json:"group"`
	Notes []string `json:"notes,omitempty"`
}

// DisplayTitle is the alias if set, else the title.
func (s Session) DisplayTitle() string {
	if s.Alias != "" {
		return s.Alias
	}
	return s.Title
}

// NeedsAttention: blocked on the user, or an unseen completion.
func (s Session) NeedsAttention() bool {
	return s.Group == GroupNeedsYou || s.Group == GroupYourTurn
}

// Snapshot is the state of every session at one instant.
type Snapshot struct {
	At         time.Time `json:"at"`
	Sessions   []Session `json:"sessions"`
	OnBattery  bool      `json:"on_battery"`
	FromDaemon bool      `json:"from_daemon"`
	TmuxOK     bool      `json:"tmux_ok"`
	Problems   []string  `json:"problems,omitempty"`
}

// Counts returns the number of sessions per group.
func (s Snapshot) Counts() map[Group]int {
	m := map[Group]int{}
	for _, x := range s.Sessions {
		m[x.Group]++
	}
	return m
}
