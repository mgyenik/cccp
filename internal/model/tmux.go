package model

import (
	"fmt"
	"time"
)

// TmuxPane is one tmux pane.
type TmuxPane struct {
	ID          string // %N
	PID         int    // pane_pid (usually the shell)
	Session     string
	WindowID    string // @N
	WindowIndex int
	WindowName  string
	PaneIndex   int
	Title       string
	Command     string
	Path        string
	// Visible: the pane's window is the current window of a session that
	// has at least one attached client.
	Visible bool
	// WindowActivity is the last time the pane's window produced output.
	WindowActivity time.Time
	WindowPanes    int
}

// Location is "session:window" (with ".pane" when the window is split).
func (p TmuxPane) Location() string {
	s := fmt.Sprintf("%s:%d", p.Session, p.WindowIndex)
	if p.WindowPanes > 1 {
		s += fmt.Sprintf(".%d", p.PaneIndex)
	}
	return s
}

// TmuxClient is one attached tmux client.
type TmuxClient struct {
	Name    string
	Session string
	Pane    string // active pane of the client's current window
	Focused bool   // terminal focus (needs focus-events on); see FocusKnown
	// FocusKnown is false when tmux cannot know terminal focus (focus-events
	// off), in which case Focused is assumed true.
	FocusKnown bool
}
