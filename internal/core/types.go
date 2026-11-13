// Package core is the functional core of cccp. Given a plain-data
// Observation of every source (hook state files, Claude's session registry,
// tmux, agent processes, transcripts) and the remembered Memory, Compute
// derives the Snapshot of all sessions. Nothing in this package performs
// I/O or reads the clock: every input is a parameter, so every rule is
// testable with literal values.
package core

import (
	"time"

	"cccp/internal/jail"
	"cccp/internal/model"
	"cccp/internal/workstream"
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
	// Fades: a best-effort cache with no TTL (Gemini's implicit caching,
	// agy), whose state is estimated: likely warm until CoolingAt, fading
	// until ExpiresAt, then taken as cold.
	Fades     bool      `json:"fades,omitempty"`
	CoolingAt time.Time `json:"cooling_at,omitzero"`
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
	// Summary is the one-line summary of the session as it is now (none
	// while one for its current state is still being written), and
	// SummaryVerdict its VerdictAct / VerdictWait.
	Summary        string `json:"summary,omitempty"`
	SummaryVerdict string `json:"summary_verdict,omitempty"`
	// AutoTurn: the last turn was started by Claude Code on a background
	// task's or subagent's report, not by the user.
	AutoTurn bool `json:"auto_turn,omitempty"`
	// BackgroundShell: the turn is over but a background shell command the
	// session started still runs (Claude Code's registry status "shell").
	BackgroundShell bool `json:"background_shell,omitempty"`
	// RecentPrompts are the user's last typed prompts, oldest first (for the
	// working title; not published).
	RecentPrompts []model.Prompt `json:"-"`
	// Placeholder is the restore placeholder pane (cccp start) waiting to
	// resume this conversation, which is not running ("" if none); its
	// window and workstream are then WindowID, Location and TmuxSess.
	Placeholder string `json:"placeholder,omitempty"`
	// RecheckAt is when the session's state may change with no new event
	// (zero: never): Compute must run again then (NextDeadline).
	RecheckAt time.Time `json:"recheck_at,omitzero"`
	Cwd       string    `json:"cwd,omitempty"`
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

// Background reports whether the session's finished turn may be waiting on
// its own background work rather than on the user: the turn answered a
// background report, or a background shell still runs. Whether it needs
// the user then is the summary's verdict (docs/notifications.md).
func (s Session) Background() bool { return s.AutoTurn || s.BackgroundShell }

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
	// Workstreams: the tmux sessions, running and dormant (docs/workstreams.md).
	Workstreams workstream.Record `json:"workstreams"`
	// Jail: the limits of tmux and its workstreams (docs/jails.md).
	Jail Jail `json:"jail"`
}

// Jail is the jails' state: whether they are on, each running workstream's
// limits and numbers, and what is to be done.
type Jail struct {
	On          bool                   `json:"on"`
	Why         string                 `json:"why,omitempty"` // why off
	Root        jail.Limits            `json:"root"`
	RootStats   jail.Stats             `json:"root_stats"`
	Workstreams []JailWorkstream       `json:"workstreams,omitempty"`
	Desired     map[string]jail.Limits `json:"desired,omitempty"` // by unit
	Moves       []jail.Move            `json:"moves,omitempty"`
	MemTotal    int64                  `json:"mem_total,omitempty"` // the machine's memory, for limits given as percentages
	Presets     map[string]jail.Limits `json:"presets,omitempty"`   // as configured
}

// JailWorkstream is one running workstream's jail.
type JailWorkstream struct {
	Name    string      `json:"name"`
	Slice   string      `json:"slice"`
	Preset  string      `json:"preset"`         // in force: normal, boost, max or custom
	Until   time.Time   `json:"until,omitzero"` // a boost's end (zero: none, or until undone)
	Boosted bool        `json:"boosted,omitempty"`
	Limits  jail.Limits `json:"limits"`
	// Normal is what it has without a boost (its configured setting), and
	// NormalPreset where that comes from: what ending a boost returns it to.
	Normal       jail.Limits `json:"normal"`
	NormalPreset string      `json:"normal_preset"`
	Stats        jail.Stats  `json:"stats"`
}

// Find returns a workstream's jail.
func (j Jail) Find(name string) (JailWorkstream, bool) {
	for _, w := range j.Workstreams {
		if w.Name == name {
			return w, true
		}
	}
	return JailWorkstream{}, false
}

// Counts returns the number of sessions per group.
func (s Snapshot) Counts() map[Group]int {
	m := map[Group]int{}
	for _, x := range s.Sessions {
		m[x.Group]++
	}
	return m
}

// agy's prompt cache, estimated. agy runs Gemini 4 models, which have only
// implicit caching: best effort, with no documented lifetime (explicit
// caching, with a TTL, "is not supported … for Gemini 4 Argon and onward":
// ai.google.dev/gemini-api/docs/caching). Measured on 8,624 agy model calls
// (2026-11-04), the chance a call's input was cached, by the time since the
// previous call: under 1 min 97–99%, 1–5 min 85–89%, 5–15 min 50–57%,
// 15–30 min 31%, 30–60 min 8%, over 1 h about 0%. So: likely warm for
// AgyWarmFor, then fading, and taken as cold after AgyColdAfter.
const (
	AgyWarmFor   = 5 * time.Minute
	AgyColdAfter = 30 * time.Minute
)

// Cooling reports whether a fading cache is past its likely-warm period at t
// (and not yet taken as cold).
func (c Cache) Cooling(t time.Time) bool {
	return c.Fades && c.Warm(t) && !t.Before(c.CoolingAt)
}
