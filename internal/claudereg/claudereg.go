// Package claudereg reads Claude Code's live session registry,
// ~/.claude/sessions/<pid>.json.
//
// The registry is not a documented-stable format (the documented interface is
// `claude agents --json`, which is too heavy to poll), so parsing is lenient:
// unknown fields are ignored and unknown status values map to Unknown.
package claudereg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cccp/internal/model"
	"cccp/internal/paths"
	"cccp/internal/procs"
)

// Entry is one registry file.
type Entry struct {
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
func (e Entry) Pane() string {
	if i := strings.LastIndexByte(e.Tmux, '%'); i >= 0 {
		return e.Tmux[i:]
	}
	return ""
}

// StatusAt is when the status last changed.
func (e Entry) StatusAt() time.Time {
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
func (e Entry) StartedAt() time.Time {
	if e.StartedAtMs == 0 {
		return time.Time{}
	}
	return time.UnixMilli(e.StartedAtMs)
}

// ProcStartTicks parses ProcStart (the /proc stat start time).
func (e Entry) ProcStartTicks() uint64 {
	v, _ := strconv.ParseUint(e.ProcStart, 10, 64)
	return v
}

// Alive reports whether the registered process is still the same process.
func (e Entry) Alive() bool { return procs.Alive(e.PID, e.ProcStartTicks()) }

// Activity maps status/waitingFor to an Activity.
func (e Entry) Activity() model.Activity {
	switch e.Status {
	case "busy":
		return model.Working
	case "idle":
		return model.Idle
	case "waiting":
		switch e.WaitingFor {
		case "permission prompt", "sandbox request":
			return model.NeedsApproval
		default: // "input needed", "worker request", "dialog open", future values
			return model.NeedsInput
		}
	}
	return model.Unknown
}

// Question describes what a waiting session waits for.
func (e Entry) Question() string {
	if e.Status == "waiting" && e.WaitingFor != "" {
		return e.WaitingFor
	}
	return ""
}

// IsUserSession reports whether the entry is a session a person works in
// (interactive or background), as opposed to an SDK or print-mode process.
// Print mode (`claude -p`) registers with kind "interactive" but entrypoint
// "sdk-cli" (observed on 2.1.316), so the entrypoint decides.
func (e Entry) IsUserSession() bool {
	if strings.HasPrefix(e.Entrypoint, "sdk") {
		return false
	}
	return e.Kind == "interactive" || e.Kind == "background"
}

// Read parses one registry file.
func Read(p string) (Entry, error) {
	var e Entry
	b, err := os.ReadFile(p)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, err
	}
	e.Path = p
	return e, nil
}

// IsEntryFile reports whether a file name in the registry dir is an entry.
func IsEntryFile(name string) bool {
	if !strings.HasSuffix(name, ".json") {
		return false
	}
	_, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
	return err == nil
}

// ReadAll parses every entry in the registry directory.
func ReadAll() []Entry {
	dir := paths.ClaudeRegistryDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Entry
	for _, d := range ents {
		if !IsEntryFile(d.Name()) {
			continue
		}
		if e, err := Read(filepath.Join(dir, d.Name())); err == nil {
			out = append(out, e)
		}
	}
	return out
}
