package model

import (
	"strconv"
	"strings"
	"time"
)

// RegistryEntry is one file of Claude Code's session registry,
// ~/.claude/sessions/<pid>.json. The format is not documented as stable, so
// unknown fields are ignored and unknown statuses map to Unknown.
type RegistryEntry struct {
	Path            string
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	StartedAtMs     int64  `json:"startedAt"`
	ProcStart       string `json:"procStart"`
	Version         string `json:"version"`
	Kind            string `json:"kind"`
	Entrypoint      string `json:"entrypoint"`
	Tmux            string `json:"tmux"` // "session:@window.%pane"
	Name            string `json:"name"`
	NameSource      string `json:"nameSource"`
	Status          string `json:"status"`
	WaitingFor      string `json:"waitingFor"`
	UpdatedAtMs     int64  `json:"updatedAt"`
	StatusUpdatedMs int64  `json:"statusUpdatedAt"`
}

// Pane returns the tmux pane id ("%21") from the Tmux field.
func (e RegistryEntry) Pane() string {
	if i := strings.LastIndexByte(e.Tmux, '%'); i >= 0 {
		return e.Tmux[i:]
	}
	return ""
}

// StatusAt is when the status last changed.
func (e RegistryEntry) StatusAt() time.Time {
	ms := e.StatusUpdatedMs
	if ms == 0 {
		ms = e.UpdatedAtMs
	}
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// StartedAt is when the session process started.
func (e RegistryEntry) StartedAt() time.Time {
	if e.StartedAtMs == 0 {
		return time.Time{}
	}
	return time.UnixMilli(e.StartedAtMs)
}

// ProcStartTicks parses ProcStart (the /proc stat start time).
func (e RegistryEntry) ProcStartTicks() uint64 {
	v, _ := strconv.ParseUint(e.ProcStart, 10, 64)
	return v
}

// Activity maps status/waitingFor to an Activity.
func (e RegistryEntry) Activity() Activity {
	switch e.Status {
	case "busy":
		return Working
	case "idle", "shell":
		// "shell": idle at the prompt while a background Bash task the
		// session started still runs (Claude Code 2.1.318: status is
		// "shell" when idle and any local_bash task is not finished).
		return Idle
	case "waiting":
		switch e.WaitingFor {
		case "permission prompt", "sandbox request":
			return NeedsApproval
		default: // "input needed", "worker request", "dialog open", future values
			return NeedsInput
		}
	}
	return Unknown
}

// BackgroundShell: idle at the prompt with a background Bash task still
// running (status "shell").
func (e RegistryEntry) BackgroundShell() bool { return e.Status == "shell" }

// Question describes what a waiting session waits for.
func (e RegistryEntry) Question() string {
	if e.Status == "waiting" && e.WaitingFor != "" {
		return e.WaitingFor
	}
	return ""
}

// IsUserSession reports whether the entry is a session a person works in
// (interactive or background), as opposed to an SDK or print-mode process.
// Print mode (`claude -p`) registers with kind "interactive" but entrypoint
// "sdk-cli" (observed on 2.1.316), so the entrypoint decides.
func (e RegistryEntry) IsUserSession() bool {
	if strings.HasPrefix(e.Entrypoint, "sdk") {
		return false
	}
	k := e.SessionKind()
	return k == "interactive" || k == "background"
}

// SessionKind is the entry's kind in cccp's terms: Claude Code writes "bg"
// for a background session (2.1.319; a conversation moved to the
// background, or started with --bg), "background" in older versions.
func (e RegistryEntry) SessionKind() string {
	if e.Kind == "bg" {
		return "background"
	}
	return e.Kind
}
