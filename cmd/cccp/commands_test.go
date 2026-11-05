package main

import (
	"flag"
	"os"
	"strings"
	"testing"
)

func TestIsTerminalRejectsDevNullAndPipes(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Fatal("/dev/null is a character device but not a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r) {
		t.Fatal("a pipe is not a terminal")
	}
}

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	d := fs.String("for", "1h", "")
	u := fs.Bool("until-undone", false, "")
	pos := parseInterspersed(fs, []string{"infra", "boost", "--for", "2h", "--until-undone"})
	if strings.Join(pos, " ") != "infra boost" || *d != "2h" || !*u {
		t.Fatalf("%v %q %v", pos, *d, *u)
	}
	fs = flag.NewFlagSet("x", flag.ContinueOnError)
	d = fs.String("for", "1h", "")
	if pos := parseInterspersed(fs, []string{"--for", "3h", "a", "max"}); strings.Join(pos, " ") != "a max" || *d != "3h" {
		t.Fatalf("%v %q", pos, *d)
	}
}
