package core

import (
	"time"

	"cccp/internal/model"
)

// Observation is everything the imperative shell read from the world, as
// plain data. It is gathered in two phases: the base fields first, then the
// transcripts that Plan says are needed.
type Observation struct {
	Hooks    []HookRecord
	Registry []RegistryRecord
	Tmux     TmuxView
	// Agents are the agent processes found in tmux panes' process trees.
	Agents []AgentProc

	OnBattery bool
	Hostname  string

	// Profiles are the known Claude config directories; exactly one is
	// the default. All config directories in this observation are cleaned
	// absolute paths ("" = the default profile).
	Profiles []model.Profile

	// AgyConversations is agy's conversation summary table, by id.
	AgyConversations map[string]model.AgyConversation

	// Conversations are every transcript on disk (the catalog), whether or
	// not a process runs it.
	Conversations []Conversation

	// Second phase, filled according to Plan's Needs.
	ClaudeTranscripts map[string]ClaudeTranscript        // by session id
	AgyTranscripts    map[string]AgyTranscript           // by conversation id
	Projects          map[ProjectKey][]ProjectTranscript // a present key means "listed" (possibly empty)

	// Problems are read errors worth showing the user.
	Problems []string
}

// HookRecord is one hook state file.
type HookRecord struct {
	Path  string
	State model.HookState
	// ProcAlive: the agent process the hook recorded (pid and start time)
	// still runs.
	ProcAlive bool
}

// RegistryRecord is one Claude Code registry file.
type RegistryRecord struct {
	Path  string
	Entry model.RegistryEntry
	// ConfigDir is the profile whose registry the file is in.
	ConfigDir string
	// Alive: the registered process (pid and start time) still runs.
	Alive bool
}

// TmuxView is the tmux server's panes and clients.
type TmuxView struct {
	OK      bool
	Err     string
	Panes   []model.TmuxPane
	Clients []model.TmuxClient
}

// AgentProc is an agent process found under a tmux pane.
type AgentProc struct {
	PID       int
	Start     uint64 // /proc start time, guards against pid reuse
	StartedAt time.Time
	Agent     model.Agent
	Pane      string
	// Depth below the pane's own process (0 = the pane process itself).
	Depth   int
	Cmdline []string
	Cwd     string
	// ConfigDir is the process's CLAUDE_CONFIG_DIR ("" = default).
	ConfigDir string
}

// Conversation is one cataloged transcript.
type Conversation struct {
	Agent       model.Agent
	SessionID   string
	ConfigDir   string // Claude profile ("" for agy)
	Path        string // the transcript
	Cwd         string
	Title       string // Claude's ai-title; agy's first prompt
	FirstPrompt string
	LastPrompt  string
	LastMessage string
	Branch      string
	Model       string
	Turns       int
	First       time.Time
	Last        time.Time
}

// ClaudeTranscript is a located and parsed Claude transcript.
type ClaudeTranscript struct {
	Path string
	Meta model.ClaudeMeta
}

// AgyTranscript is a located and parsed agy transcript.
type AgyTranscript struct {
	Path string
	Meta model.AgyMeta
}

// ProjectTranscript is one transcript in a Claude project directory, used to
// match an unmanaged session's pane title to its conversation.
type ProjectTranscript struct {
	SessionID string
	Path      string
	ModTime   time.Time
	Meta      model.ClaudeMeta
}

// ProjectKey names a Claude project directory: a working directory within
// a profile.
type ProjectKey struct {
	ConfigDir string // absolute; never "" (the default profile's directory is used)
	Cwd       string
}

// Needs lists the transcripts Compute will use, so the shell reads only
// those.
type Needs struct {
	Claude []ClaudeNeed
	Agy    []AgyNeed
	// Projects are project directories whose transcripts must be listed
	// (to match unmanaged sessions by title).
	Projects []ProjectKey
}

// ClaudeNeed asks for one Claude transcript.
type ClaudeNeed struct {
	SessionID string
	ConfigDir string // the session's profile (absolute)
	Cwd       string // where to look first
	Hint      string // a path the hook reported, if any
}

// AgyNeed asks for one agy transcript.
type AgyNeed struct {
	ConversationID string
	Hint           string
}

// HookChange says what a hook state file change implies.
type HookChange struct {
	// Rediscover: a session appeared or disappeared, or moved to another
	// process or pane.
	Rediscover bool
	// Ended: the session just ended (its transcript is final).
	Ended bool
}
