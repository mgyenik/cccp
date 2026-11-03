package nav

import "testing"

func TestCyclingKeepsTheReturnPoint(t *testing.T) {
	var s State
	s = Jumped(s, "%home", "%a") // first jump: remember home
	s = Jumped(s, "%a", "%b")    // pressed again from where it landed: a cycle
	s = Jumped(s, "%b", "%c")
	if s != (State{Back: "%home", Landed: "%c"}) {
		t.Fatalf("%+v", s)
	}
	target, s, ok := Return(s, "%c")
	if !ok || target != "%home" {
		t.Fatalf("back went to %q", target)
	}
	// Back again toggles to where back was pressed.
	if target, _, ok := Return(s, "%home"); !ok || target != "%c" {
		t.Fatalf("toggle went to %q", target)
	}
}

func TestMovingOnStartsANewCycle(t *testing.T) {
	s := Jumped(State{}, "%home", "%a")
	// The user moved to another window with their own keys, then jumped.
	s = Jumped(s, "%other", "%b")
	if s.Back != "%other" {
		t.Fatalf("a jump from elsewhere must record where it started: %+v", s)
	}
}

func TestAfterReturningAJumpRecordsHome(t *testing.T) {
	s := Jumped(State{}, "%home", "%a")
	_, s, _ = Return(s, "%a")
	// Back cleared Landed, so a jump from home records home again.

	s = Jumped(s, "%home", "%b")
	if s != (State{Back: "%home", Landed: "%b"}) {
		t.Fatalf("%+v", s)
	}
	// After toggling back to a session, that session is where you chose to
	// be: a jump from it records it.

	_, s, _ = Return(s, "%b")    // home
	_, s, _ = Return(s, "%home") // toggled to %b
	s = Jumped(s, "%b", "%c")    // a jump from %b: %b is where you chose to be
	if s.Back != "%b" {
		t.Fatalf("%+v", s)
	}
}

func TestNowhereToReturnTo(t *testing.T) {
	if _, _, ok := Return(State{}, "%a"); ok {
		t.Fatal("no jump yet")
	}
	if _, _, ok := Return(State{Back: "%a"}, "%a"); ok {
		t.Fatal("already there")
	}
	// A jump that did not move (already on the target) changes nothing,
	// also from a pane cccp did not put you on: the return point survives.
	s := State{Back: "%home", Landed: "%a"}
	if got := Jumped(s, "%a", "%a"); got != s {
		t.Fatalf("%+v", got)
	}
	if got := Jumped(s, "%x", "%x"); got != s {
		t.Fatalf("jumping to where you are wiped the return point: %+v", got)
	}
	// Without knowing where the client was, the return point is kept.
	if got := Jumped(s, "", "%b"); got.Back != "%home" || got.Landed != "%b" {
		t.Fatalf("%+v", got)
	}
}

func TestEncoding(t *testing.T) {
	for _, s := range []State{{}, {Back: "%1"}, {Back: "%1", Landed: "%22"}, {Landed: "%3"}} {
		if got := Decode(Encode(s)); got != s {
			t.Errorf("%+v → %q → %+v", s, Encode(s), got)
		}
	}
	if Encode(State{}) != "" {
		t.Fatal("the empty state unsets the option")
	}
	if got := Option("/dev/pts/3"); got != "@cccp_nav_dev_pts_3" {
		t.Fatal(got)
	}
}
