package tmux

import (
	"reflect"
	"strings"
	"testing"
)

func TestPopupArgs(t *testing.T) {
	got := PopupArgs(Popup{
		Client: "/dev/pts/3", Width: "92%", Height: "88%", Title: " cccp ",
		Env:  []string{"CCCP_SOCKET=/run/x.sock"},
		Argv: []string{"/home/user/.local/bin/cccp", "ui", "--client", "/dev/pts/3", "it's"},
	})
	want := []string{"display-popup", "-E", "-c", "/dev/pts/3", "-w", "92%", "-h", "88%", "-T", " cccp ",
		"-e", "CCCP_SOCKET=/run/x.sock",
		`'/home/user/.local/bin/cccp' 'ui' '--client' '/dev/pts/3' 'it'\''s'`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestDisplayPopupNeedsClient(t *testing.T) {
	if err := New("/nonexistent").DisplayPopup(Popup{Argv: []string{"true"}}); err == nil {
		t.Fatal("a popup without an explicit client must be refused")
	}
}

func TestParseClients(t *testing.T) {
	line := func(f ...string) string { return strings.Join(f, sep) }
	out := strings.Join([]string{
		line("/dev/pts/2", "beta", "%8", "attached,focused,UTF-8", "1793690287", "1", "1793690000"),
		line("/dev/pts/0", "alpha", "%2", "attached,UTF-8", "1793690400", "1", "1793690000"),
		line("/dev/pts/9", "x", "%1", "attached,focused", "1793690300", "0", "1793690000"), // focus-events off
		"garbage",
	}, "\n") + "\n"
	cs := ParseClients(out)
	if len(cs) != 3 {
		t.Fatalf("%d clients", len(cs))
	}
	if c := cs[0]; c.Name != "/dev/pts/2" || c.Session != "beta" || c.Pane != "%8" || !c.Focused ||
		c.Created.Unix() != 1793690287 || c.FocusSince.Unix() != 1793690000 || !c.Looking() {
		t.Fatalf("%+v", c)
	}
	if c := cs[1]; c.Focused || !c.FocusReported() {
		t.Fatalf("attached after: reports, not focused: %+v", c)
	}
	if c := cs[2]; !c.FocusSince.IsZero() || c.Looking() {
		t.Fatalf("focus-events off: focus is not reported: %+v", c)
	}
	// Unset option, bad numbers: unknown, never a time near 1970 that would
	// trust every client.
	for _, bad := range []string{"", "x", "0", "-5"} {
		if cs := ParseClients(line("/dev/pts/1", "s", "%1", "focused", bad, "1", bad) + "\n"); !cs[0].Created.IsZero() || !cs[0].FocusSince.IsZero() || cs[0].Looking() {
			t.Fatalf("%q: %+v", bad, cs[0])
		}
	}
}
