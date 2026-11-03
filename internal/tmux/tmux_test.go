package tmux

import (
	"reflect"
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
