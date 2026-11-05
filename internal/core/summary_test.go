package core

import (
	"strings"
	"testing"
)

func TestSummaryInputTailKeepsUTF8(t *testing.T) {
	s := Session{Title: "t", LastMessage: strings.Repeat("é", 4000)}
	in, hash := SummaryInput(s)
	if !strings.Contains(in, "…") || strings.ContainsRune(in, '�') || len(hash) != 16 {
		t.Fatalf("hash %q, invalid UTF-8 or no ellipsis", hash)
	}
}

// Each state has its own input: the turn's origin, and a closed
// conversation versus a live session, change it.
func TestSummaryInputDistinguishesStates(t *testing.T) {
	s := Session{Title: "t", Group: GroupYourTurn, LastPrompt: "p", LastMessage: "m"}
	_, user := SummaryInput(s)
	s.AutoTurn = true
	in, auto := SummaryInput(s)
	if user == auto || !strings.Contains(in, "not started by the user") || strings.Contains(in, "User's last prompt") {
		t.Fatalf("auto turn: %s", in)
	}
	s.AutoTurn, s.Group = false, GroupClosed
	if in, closed := SummaryInput(s); closed == user || !strings.HasPrefix(in, "Conversation title") {
		t.Fatalf("closed: %s", in)
	}
}
