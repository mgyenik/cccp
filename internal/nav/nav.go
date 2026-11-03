// Package nav is the "back" key's state machine: per tmux client, where to
// return to after cccp jumped somewhere. Pure.
//
// The rule that matters: pressing the jump key repeatedly cycles through
// the sessions that need you, and back must then return to where you were
// before the first of those jumps, not to the previous session in the
// cycle. So the state records where cccp's last jump landed: a jump made
// from there continues a cycle and keeps the return point; a jump from
// anywhere else (you moved on, or came back) starts a new one.
package nav

import (
	"regexp"
	"strings"
)

// State is one client's navigation state: tmux pane ids, "" when unset.
type State struct {
	Back   string // where back returns to
	Landed string // where cccp's last jump put the client
}

// Jumped is the state after cccp switched the client from pane from to
// pane to.
func Jumped(s State, from, to string) State {
	if from == to {
		return s // already there: nothing moved, keep the return point
	}
	if from != "" && from != s.Landed {
		s.Back = from
	}
	s.Landed = to
	return s
}

// Return plans back from pane from: the pane to switch to and the state
// once there. Back is a toggle: pressing it again returns to from. ok is
// false when there is nowhere to go back to.
func Return(s State, from string) (target string, next State, ok bool) {
	if s.Back == "" || s.Back == from {
		return "", s, false
	}
	// Landed is cleared: the client is back where it chose to be, so a jump
	// from here starts a new cycle and records this pane.
	return s.Back, State{Back: from}, true
}

// Encode and Decode store a State in one tmux option value.
func Encode(s State) string {
	if s == (State{}) {
		return ""
	}
	return s.Back + "," + s.Landed
}

func Decode(v string) State {
	back, landed, _ := strings.Cut(strings.TrimSpace(v), ",")
	return State{Back: back, Landed: strings.TrimSpace(landed)}
}

var notOption = regexp.MustCompile(`[^A-Za-z0-9]+`)

// Option is the tmux user option holding a client's state. tmux client
// names are tty paths (/dev/pts/3), not valid in an option name.
func Option(client string) string {
	return "@cccp_nav_" + strings.Trim(notOption.ReplaceAllString(client, "_"), "_")
}
