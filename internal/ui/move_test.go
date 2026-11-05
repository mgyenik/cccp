package ui

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"cccp/internal/core"
	"cccp/internal/workstream"
)

func targetNames(ts []moveTarget) []string {
	var out []string
	for _, t := range ts {
		s := t.Name
		if t.New {
			s = "+" + s
		}
		if t.Dormant {
			s += "(dormant)"
		}
		out = append(out, s)
	}
	return out
}

func TestMoveTargets(t *testing.T) {
	all := []moveTarget{{Name: "infra"}, {Name: "docs", Dormant: true}, {Name: "doc-site"}}
	// Nothing typed: every workstream, running before dormant.
	if got := targetNames(moveTargets("", all)); !reflect.DeepEqual(got, []string{"doc-site", "infra", "docs(dormant)"}) {
		t.Fatalf("%v", got)
	}
	// A new name comes after the matches; Enter takes the best match.
	if got := targetNames(moveTargets("doc", all)); len(got) != 3 || got[2] != "+doc" {
		t.Fatalf("%v", got)
	}
	// An exact existing name offers no new one.
	if got := targetNames(moveTargets("infra", all)); !reflect.DeepEqual(got, []string{"infra"}) {
		t.Fatalf("%v", got)
	}
	// A name tmux would mangle is not offered as new.
	if got := targetNames(moveTargets("v1.2", all)); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// Nothing matches: only the new one.
	if got := targetNames(moveTargets("api", all)); !reflect.DeepEqual(got, []string{"+api"}) {
		t.Fatalf("%v", got)
	}
}

func TestMoverLeavesOutTheSessionsOwnWorkstream(t *testing.T) {
	snap := core.Snapshot{Workstreams: workstream.Record{Workstreams: []workstream.Workstream{{Name: "infra"}, {Name: "docs"}}}}
	m := newMover(core.Session{TmuxSess: "infra", Alive: true, Pane: "%1"}, snap)
	if got := targetNames(m.choices); !reflect.DeepEqual(got, []string{"docs"}) {
		t.Fatalf("%v", got)
	}
}

func TestCockpitMove(t *testing.T) {
	m, _, src := newModel(t)
	type call struct{ key, dest string }
	var calls []call
	m.opt.Actions.MoveSession = func(s core.Session, dest string) error {
		calls = append(calls, call{s.Key, dest})
		if dest == "broken" {
			return errors.New("tmux said no")
		}
		return nil
	}
	snap := snapshot()
	snap.Workstreams = workstream.Record{Workstreams: []workstream.Workstream{{Name: "docs"}}}
	m.Update(snapMsg(snap))
	key(m, "M")
	if m.mode != modeMove || !strings.Contains(m.render(), "Move") {
		t.Fatal("M opens the move picker")
	}
	typeText(m, "api")
	key(m, "enter") // nothing matches "api": the new workstream
	if !reflect.DeepEqual(calls, []call{{"claude:a", "api"}}) {
		t.Fatalf("calls %v", calls)
	}
	if m.quitting || m.mode != modeList || !strings.Contains(m.status, `moved "Approve deploy" to api`) || src.refreshed != 1 {
		t.Fatalf("stays open with a status: quitting %v mode %v status %q refreshed %d", m.quitting, m.mode, m.status, src.refreshed)
	}
	// A failed move is reported; the cockpit stays.
	key(m, "M")
	typeText(m, "broken")
	key(m, "enter")
	if m.quitting || !strings.Contains(m.status, "tmux said no") {
		t.Fatalf("status %q", m.status)
	}
	// Esc cancels without moving.
	key(m, "M")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.mode != modeList || len(calls) != 2 {
		t.Fatalf("mode %v calls %v", m.mode, calls)
	}
}
