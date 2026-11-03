package model

import (
	"strings"
	"testing"
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
