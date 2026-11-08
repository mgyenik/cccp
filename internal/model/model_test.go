package model

import (
	"strings"
	"testing"
	"time"
)

func TestIsPrintMode(t *testing.T) {
	cases := map[string]bool{
		"claude":                          false,
		"claude --resume abc":             false,
		"claude -p hello":                 true,
		"claude --model haiku --print hi": true,
		"agy --prompt=hi":                 true,
		"agy -i hello":                    false, // --prompt-interactive
		"claude -- -p":                    false, // after --, a prompt
		"/home/user/.local/bin/claude --resume x -p": true,
	}
	for in, want := range cases {
		if got := IsPrintMode(strings.Fields(in)); got != want {
			t.Errorf("%q: got %v want %v", in, got, want)
		}
	}
}

func TestProfileEnvironSelectsTheDefaultByRemovingTheVariable(t *testing.T) {
	environ := []string{"HOME=/h", "CLAUDE_CONFIG_DIR=/h/.claude-work", "CLAUDE_CONFIG_DIRX=keep"}
	def := Profile{Dir: "/h/.claude", Default: true}
	if got := strings.Join(def.Environ(environ), " "); got != "HOME=/h CLAUDE_CONFIG_DIRX=keep" {
		t.Fatalf("default: %q", got)
	}
	work := Profile{Dir: "/h/.claude-work"}
	if got := strings.Join(work.Environ(environ), " "); got != "HOME=/h CLAUDE_CONFIG_DIRX=keep CLAUDE_CONFIG_DIR=/h/.claude-work" {
		t.Fatalf("other: %q", got)
	}
	if len(environ) != 3 || environ[1] != "CLAUDE_CONFIG_DIR=/h/.claude-work" {
		t.Fatal("the input environment was modified")
	}
}

func TestTmuxServers(t *testing.T) {
	if got := TmuxSocketFromEnv("/tmp/tmux-1000/default,1646894,0"); got != "/tmp/tmux-1000/default" {
		t.Fatal(got)
	}
	if TmuxSocketFromEnv("") != "" {
		t.Fatal("not in tmux")
	}
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"/tmp/tmux-1000/default", "/tmp/tmux-1000/default", true},
		{"/tmp/tmux-1000/default", "/tmp/tmux-1000//default", true},
		{"/tmp/tmux-1000/default", "/tmp/tmux-1000/work", false},
		{"", "/tmp/tmux-1000/work", true}, // unknown: nothing to compare
		{"/tmp/tmux-1000/work", "", true},
	} {
		if SameTmuxServer(c.a, c.b) != c.same {
			t.Errorf("SameTmuxServer(%q, %q) want %v", c.a, c.b, c.same)
		}
	}
}

func TestFocusIsTrustedOnlyFromClientsAskedForIt(t *testing.T) {
	since := time.Unix(1000, 0)
	c := TmuxClient{Focused: true, FocusSince: since}
	for _, x := range []struct {
		created time.Time
		looking bool
		why     string
	}{
		{time.Unix(999, 0), false, "attached before focus reporting: tmux's initial flag"},
		{time.Unix(1000, 0), true, "attached the same second"},
		{time.Unix(2000, 0), true, "attached after"},
		{time.Time{}, false, "attach time unknown"},
	} {
		c.Created = x.created
		if c.Looking() != x.looking {
			t.Errorf("%s: looking %v", x.why, c.Looking())
		}
	}
	c.Created, c.FocusSince = time.Unix(2000, 0), time.Time{}
	if c.Looking() {
		t.Error("focus reporting not known to be on")
	}
	c.FocusSince, c.Focused = since, false
	if c.Looking() || !c.FocusReported() {
		t.Error("a reporting client that lost focus is not looking")
	}
}

func TestFocusSince(t *testing.T) {
	start, now := time.Unix(100, 0), time.Unix(900, 0)
	if !FocusSince(0, start, now).Equal(start) {
		t.Fatal("no client attached: since the server started")
	}
	if !FocusSince(2, start, now).Equal(now) {
		t.Fatal("clients attached: from now")
	}
}

func TestRegistrySessionKind(t *testing.T) {
	for kind, want := range map[string]string{"interactive": "interactive", "bg": "background", "background": "background", "daemon": "daemon"} {
		e := RegistryEntry{Kind: kind, Entrypoint: "cli"}
		if got := e.SessionKind(); got != want {
			t.Errorf("%s: %s", kind, got)
		}
		if e.IsUserSession() != (want == "interactive" || want == "background") {
			t.Errorf("%s: user session %v", kind, e.IsUserSession())
		}
	}
	if (RegistryEntry{Kind: "bg", Entrypoint: "sdk-ts"}).IsUserSession() {
		t.Fatal("an SDK process is not a user's session")
	}
}
