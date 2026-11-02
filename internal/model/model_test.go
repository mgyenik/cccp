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
