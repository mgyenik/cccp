package procs

import (
	"os"
	"strings"
	"testing"
)

func TestParseStatCommWithSpacesAndParens(t *testing.T) {
	line := "1234 (my (weird) proc) S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 98765 1000 10 18446744073709551615"
	st, err := parseStat(1234, []byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if st.Comm != "my (weird) proc" || st.State != 'S' || st.PPID != 1 || st.StartTime != 98765 {
		t.Fatalf("%+v", st)
	}
}

func TestAliveSelf(t *testing.T) {
	st, err := ReadStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !Alive(st.PID, st.StartTime) {
		t.Fatal("self not alive")
	}
	if Alive(st.PID, st.StartTime+1) {
		t.Fatal("start time mismatch must mean a different process")
	}
}

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
