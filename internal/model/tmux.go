package model

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// TmuxSocketFromEnv is the server socket in a $TMUX value
// ("/tmp/tmux-1000/default,1646894,0"), "" when not in tmux.
func TmuxSocketFromEnv(v string) string {
	sock, _, _ := strings.Cut(v, ",")
	return sock
}

// SameTmuxServer reports whether a pane id claimed on server claimed can be
// looked up on server ours. Pane ids are per server: %3 on one server is
// unrelated to %3 on another. An unknown server on either side (state
// written before cccp recorded servers) is taken to be the same, as there
// is nothing to compare.
func SameTmuxServer(claimed, ours string) bool {
	return claimed == "" || ours == "" || filepath.Clean(claimed) == filepath.Clean(ours)
}

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

	// For workstreams (recording and restoring sessions):
	SessionID       string    // $N
	SessionActivity time.Time // last use of the session
	WindowActive    bool      // the current window of its session
	WindowLayout    string    // #{window_layout}
	WindowAutoName  bool      // automatic-rename is on for the window
	PaneActive      bool      // the active pane of its window
	Resume          string    // the @cccp_resume pane option (a restore placeholder)
	// The server: its pid and start time (with the socket, its identity).
	ServerPID   int
	ServerStart int64
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
	// Focused is tmux's focus flag for the client's terminal. It means
	// something only if FocusReported: tmux starts every client focused and
	// clears the flag only when the terminal reports losing focus.
	Focused bool
	// Created is when the client attached.
	Created time.Time
	// FocusSince is when the server started asking terminals for focus
	// reports (cccp's @cccp_focus_since, set while focus-events is on);
	// zero when unknown or focus-events is off.
	FocusSince time.Time
}

// FocusReported: tmux asked this client's terminal for focus reports. tmux
// asks only when a terminal attaches, and only if focus-events is on at
// that moment (tty_update_features in tmux's tty.c); turning the option on
// later does not ask terminals already attached. So a client that attached
// before FocusSince never reports focus, and its Focused stays at tmux's
// initial "focused" forever.
func (c TmuxClient) FocusReported() bool {
	return !c.FocusSince.IsZero() && !c.Created.Before(c.FocusSince) // an unknown (zero) Created is before
}

// Looking: the client's terminal has focus, as far as can be known. A
// client whose focus is not reported counts as not looking: a notification
// for a session on screen is a nuisance, one silenced for a session nobody
// is looking at is a miss.
func (c TmuxClient) Looking() bool { return c.Focused && c.FocusReported() }

// FocusSince decides @cccp_focus_since when cccp's tmux snippet is sourced
// and it is not set yet: when the server started, if no client is attached
// (the snippet is being loaded at server start, so every client to come
// attaches with focus-events on); otherwise now, as the attached clients'
// terminals may never have been asked (focus-events was off, or turned on
// after they attached) and there is no way to tell.
func FocusSince(attached int, serverStart, now time.Time) time.Time {
	if attached == 0 {
		return serverStart
	}
	return now
}
