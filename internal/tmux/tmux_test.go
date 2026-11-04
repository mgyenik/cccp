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

func TestParsePanes(t *testing.T) {
	line := strings.Join([]string{"%3", "123", "infra", "@2", "1", "claude", "0", "title", "claude", "/w/infra",
		"1", "1", "1793690000", "2", "$4", "1793690100", "b25d,80x24,0,0,0", "1", "1",
		`{"agent":"claude","session_id":"x"}`, "999", "1793000000"}, sep)
	ps := ParsePanes(line + "\n" + "short" + sep + "line\n")
	if len(ps) != 1 {
		t.Fatalf("%d panes", len(ps))
	}
	p := ps[0]
	if p.ID != "%3" || p.Session != "infra" || p.SessionID != "$4" || p.SessionActivity.Unix() != 1793690100 ||
		!p.WindowActive || p.WindowLayout != "b25d,80x24,0,0,0" || !p.WindowAutoName || !p.PaneActive ||
		p.Resume != `{"agent":"claude","session_id":"x"}` || p.ServerPID != 999 || p.ServerStart != 1793000000 ||
		!p.Visible || p.WindowPanes != 2 {
		t.Fatalf("%+v", p)
	}
}

func TestParseBases(t *testing.T) {
	if b, err := ParseBases("1\n0\n"); err != nil || b.Window != 1 || b.Pane != 0 {
		t.Fatal(b, err)
	}
	for _, bad := range []string{"", "1\n", "a\nb\n", "1\n2\n3\n"} {
		if _, err := ParseBases(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
