package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cccp/internal/model"
	"cccp/internal/workstream"
)

// sandbox starts a tmux server on a socket in a temp directory, configured
// only by conf (never ~/.tmux.conf), and returns a client for it.
func sandbox(t *testing.T, conf string) (*Client, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "tmux.sock")
	cfg := filepath.Join(dir, "tmux.conf")
	if err := os.WriteFile(cfg, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	// The bootstrap session keeps the server up while the test runs.
	if out, err := exec.Command("tmux", "-S", sock, "-f", cfg, "new-session", "-d", "-s", "bootstrap", "sleep 60").CombinedOutput(); err != nil {
		t.Fatalf("starting tmux: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", sock, "kill-server").Run() })
	return New(sock), dir
}

func TestRestoreRecreatesAWorkstream(t *testing.T) {
	c, dir := sandbox(t, "set -g base-index 1\nset -g exit-empty off\n")
	for _, d := range []string{"a", "b", "c"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The placeholder: any program that stays up will do (cccp's own is
	// tested in cmd); it ignores the resume-pane arguments.
	bin := filepath.Join(dir, "placeholder")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A layout as tmux records it (its checksum is checked): two panes side
	// by side in a 160x40 window.
	if _, err := c.Run("new-window", "-d", "-t", "=bootstrap:", "-n", "probe", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run("resize-window", "-t", "=bootstrap:probe", "-x", "160", "-y", "40"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run("split-window", "-h", "-d", "-t", "=bootstrap:probe", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	layout, err := c.Run("display-message", "-p", "-t", "=bootstrap:probe", "#{window_layout}")
	if err != nil {
		t.Fatal(err)
	}
	layout = strings.TrimSpace(layout)
	conv := workstream.Conversation{Agent: model.Claude, SessionID: "c1", Title: "Fix it"}
	w := workstream.Workstream{Name: "infra", Windows: []workstream.Window{
		{Index: 2, Name: "logs", AutoName: false, Layout: layout,
			Panes: []workstream.Pane{
				{Cwd: filepath.Join(dir, "a"), Kind: workstream.KindShell},
				{Cwd: filepath.Join(dir, "b"), Kind: workstream.KindConversation, Conv: conv, Active: true}}},
		{Index: 5, AutoName: true, Layout: "bbbb,160x40,0,0,3", Active: true, // one pane: no layout to apply
			Panes: []workstream.Pane{{Cwd: filepath.Join(dir, "c"), Kind: workstream.KindShell}}},
	}}
	bases, err := c.Bases()
	if err != nil || bases.Window != 1 {
		t.Fatalf("bases %+v %v", bases, err)
	}
	if err := c.Restore(workstream.Plan(w, bases, bin)); err != nil {
		t.Fatal(err)
	}
	panes, err := c.ListPanes()
	if err != nil {
		t.Fatal(err)
	}
	// One line per restored pane: window, pane, directory, how the window is
	// named, what is active, and whether the pane carries a resume mark.
	var got []string
	for _, p := range panes {
		if p.Session != "infra" {
			continue
		}
		naming := "auto"
		if !p.WindowAutoName {
			naming = "named " + p.WindowName
		}
		got = append(got, fmt.Sprintf("%d.%d %s %s window-active=%v pane-active=%v marked=%v",
			p.WindowIndex, p.PaneIndex, filepath.Base(p.Path), naming, p.WindowActive, p.PaneActive, p.Resume != ""))
	}
	want := []string{
		"2.0 a named logs window-active=false pane-active=false marked=false",
		"2.1 b named logs window-active=false pane-active=true marked=true",
		"5.0 c auto window-active=true pane-active=true marked=false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restored:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The recorded layout is applied: two panes side by side, 80 and 79 wide.
	sizes, err := c.Run("list-panes", "-t", "=infra:2", "-F", "#{pane_width}x#{pane_height}")
	if err != nil || strings.Join(strings.Fields(sizes), " ") != "80x40 79x40" {
		t.Fatalf("layout: %q %v", sizes, err)
	}
	// The mark is the conversation, as the record reads it back.
	for _, p := range panes {
		if p.Resume != "" {
			if c, ok := workstream.ParseTag(p.Resume); !ok || c.SessionID != "c1" || c.Title != "Fix it" {
				t.Fatalf("mark %q", p.Resume)
			}
		}
	}
}

// A layout tmux rejects (a damaged record) only loses the arrangement: the
// windows and panes are restored all the same.
func TestRestorePassesOverARejectedLayout(t *testing.T) {
	c, dir := sandbox(t, "set -g exit-empty off\n")
	w := workstream.Workstream{Name: "x", Windows: []workstream.Window{{Index: 0, AutoName: true,
		Layout: "zzzz,160x40,0,0{80x40,0,0,1,79x40,81,0,2}", // not a valid checksum
		Panes:  []workstream.Pane{{Cwd: dir, Kind: workstream.KindShell}, {Cwd: dir, Kind: workstream.KindShell}}}}}
	bases, err := c.Bases()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Restore(workstream.Plan(w, bases, "unused")); err != nil {
		t.Fatalf("a rejected layout failed the restore: %v", err)
	}
	if out, err := c.Run("list-panes", "-t", "=x:", "-F", "#{pane_id}"); err != nil || len(strings.Fields(out)) != 2 {
		t.Fatalf("panes: %q %v", out, err)
	}
}
