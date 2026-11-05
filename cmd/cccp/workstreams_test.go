package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"cccp/internal/core"
	"cccp/internal/ipc"
	"cccp/internal/memfile"
	"cccp/internal/model"
	"cccp/internal/paths"
	"cccp/internal/store"
	"cccp/internal/tmux"
	"cccp/internal/workstream"
)

// testApp is an app on a sandboxed tmux server: every path (state, cache,
// sockets, home) in a temp directory, no daemon, and a stand-in claude on
// the server's PATH.
func testApp(t *testing.T, conf ...string) (*app, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{home, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"CCCP_STATE_DIR": filepath.Join(dir, "state"), "CCCP_CACHE_DIR": filepath.Join(dir, "cache"),
		"CCCP_SOCKET": filepath.Join(dir, "cccp.sock"), "CCCP_TMUX_SOCKET": filepath.Join(dir, "tmux.sock"),
		"XDG_CONFIG_HOME": filepath.Join(dir, "config"),
	}
	l := paths.Resolve(func(k string) string { return env[k] }, home, os.Getuid())
	// The default shell records each start, so a test can tell whether an
	// action started a shell.
	shell := filepath.Join(bin, "shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\necho started >> "+filepath.Join(dir, "shell-starts")+"\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "tmux.conf")
	lines := append([]string{"set -g exit-empty off", "set -g default-command " + shell}, conf...)
	if err := os.WriteFile(cfg, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	boot := exec.Command("tmux", "-S", l.TmuxSocket, "-f", cfg, "start-server")
	boot.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := boot.CombinedOutput(); err != nil {
		t.Fatalf("starting tmux: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", l.TmuxSocket, "kill-server").Run() })
	a := &app{l: l, tmux: tmux.New(l.TmuxSocket), daemon: ipc.Client{Socket: l.Socket},
		store: store.Store{Dir: l.SessionsDir}, stderr: io.Discard}
	return a, dir
}

// sessionIn is the cockpit's row for whatever runs in the first pane of
// session:window.
func sessionIn(t *testing.T, a *app, target string) core.Session {
	t.Helper()
	out, err := a.tmux.Run("display-message", "-p", "-t", target, "#{pane_id} #{window_id} #{session_name} #{pane_current_path}")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(out)
	return core.Session{Key: "claude:x", Agent: model.Claude, SessionID: "x", Alive: true,
		Pane: f[0], WindowID: f[1], TmuxSess: f[2], Cwd: f[3], Title: "T"}
}

// layout is "session: window-name…" for every session, sorted.
func layout(t *testing.T, a *app) string {
	t.Helper()
	out, err := a.tmux.Run("list-windows", "-a", "-F", "#{session_name}:#{window_name}")
	if err != nil {
		return ""
	}
	lines := strings.Fields(out)
	sort.Strings(lines)
	return strings.Join(lines, " ")
}

func TestMoveIntoAnExistingWorkstream(t *testing.T) {
	a, dir := testApp(t)
	run := func(args ...string) {
		if _, err := a.tmux.Run(args...); err != nil {
			t.Fatal(err)
		}
	}
	run("new-session", "-d", "-s", "infra", "-n", "keep", "-c", dir)
	run("new-window", "-d", "-t", "=infra:", "-n", "mover", "-c", dir)
	run("new-session", "-d", "-s", "docs", "-n", "docs", "-c", dir)
	if err := a.moveSession(sessionIn(t, a, "=infra:mover"), "docs"); err != nil {
		t.Fatal(err)
	}
	if got := layout(t, a); got != "docs:docs docs:mover infra:keep" {
		t.Fatalf("%s", got)
	}
	// Into its own workstream: refused.
	if err := a.moveSession(sessionIn(t, a, "=docs:mover"), "docs"); err == nil || !strings.Contains(err.Error(), "already in docs") {
		t.Fatalf("%v", err)
	}
}

// shellStarts counts the default shells started so far.
func shellStarts(dir string) int {
	b, _ := os.ReadFile(filepath.Join(dir, "shell-starts"))
	return strings.Count(string(b), "started")
}

func TestMoveTheLastWindowIntoANewWorkstream(t *testing.T) {
	a, dir := testApp(t, "set -g base-index 1")
	proj := filepath.Join(dir, "proj")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := a.tmux.Run("new-session", "-d", "-s", "old", "-n", "only", "-c", proj); err != nil {
		t.Fatal(err)
	}
	if _, err := a.tmux.Run("new-session", "-d", "-s", "other", "-n", "o", "-c", dir); err != nil {
		t.Fatal(err)
	}
	before := shellStarts(dir)
	if err := a.moveSession(sessionIn(t, a, "=old:only"), "api"); err != nil {
		t.Fatal(err)
	}
	// The window tmux makes the new session start with ran no shell (one
	// killed mid-start can leave locks behind), and the moved window took
	// its place: the session's first index.
	if n := shellStarts(dir); n != before {
		t.Fatalf("the move started %d shell(s)", n-before)
	}
	if out, _ := a.tmux.Run("list-windows", "-t", "=api", "-F", "#{window_index}"); strings.TrimSpace(out) != "1" {
		t.Fatalf("moved window at index %q, want the first (base-index 1)", out)
	}
	// The window is api's only one (no scratch window left), and old, now
	// empty, has closed.
	if got := layout(t, a); got != "api:only other:o" {
		t.Fatalf("%s", got)
	}
	// The new workstream starts in the session's directory (its new windows
	// open there).
	if out, _ := a.tmux.Run("display-message", "-p", "-t", "=api:", "#{session_path}"); strings.TrimSpace(out) != proj {
		t.Fatalf("session path %q", out)
	}
}

func TestMoveIntoADormantWorkstreamRestoresIt(t *testing.T) {
	a, dir := testApp(t)
	if _, err := a.tmux.Run("new-session", "-d", "-s", "infra", "-n", "w", "-c", dir); err != nil {
		t.Fatal(err)
	}
	// docs was recorded on a server that is gone.
	rec := workstream.Record{Workstreams: []workstream.Workstream{{Name: "docs", ID: "$9",
		Server: workstream.Server{PID: 1, Start: 1}, LastUsed: time.Now(),
		Windows: []workstream.Window{{Index: 0, Name: "notes", Panes: []workstream.Pane{{Cwd: dir, Kind: workstream.KindShell}}}}}}}
	mem := core.NewMemory()
	mem.Workstreams = rec
	if err := (memfile.File{Path: a.l.MemoryFile, SummariesPath: a.l.SummariesFile, WorkstreamsPath: a.l.WorkstreamsFile}).Save(mem); err != nil {
		t.Fatal(err)
	}
	if err := a.moveSession(sessionIn(t, a, "=infra:w"), "docs"); err != nil {
		t.Fatal(err)
	}
	if got := layout(t, a); got != "docs:notes docs:w" {
		t.Fatalf("restored, then moved into: %s", got)
	}
}

func TestMoveAClosedConversationIntoANewWorkstream(t *testing.T) {
	a, dir := testApp(t)
	if _, err := a.tmux.Run("new-session", "-d", "-s", "keep", "-c", dir); err != nil {
		t.Fatal(err)
	}
	before := shellStarts(dir)
	closed := core.Session{Key: "claude:c1", Agent: model.Claude, SessionID: "c1", Cwd: dir, DefaultProfile: true, Title: "Old"}
	if err := a.moveSession(closed, "fresh"); err != nil {
		t.Fatal(err)
	}
	out, err := a.tmux.Run("list-panes", "-s", "-t", "=fresh", "-F", "#{window_index} #{pane_start_command}")
	if err != nil || !strings.HasPrefix(out, "0 ") || strings.Count(out, "\n") != 1 || !strings.Contains(out, "claude --resume c1") {
		t.Fatalf("panes:\n%s %v", out, err)
	}
	if n := shellStarts(dir); n != before {
		t.Fatalf("the move started %d shell(s)", n-before)
	}
}

func TestMoveAClosedConversationOpensItThere(t *testing.T) {
	a, dir := testApp(t)
	if _, err := a.tmux.Run("new-session", "-d", "-s", "docs", "-n", "docs", "-c", dir); err != nil {
		t.Fatal(err)
	}
	closed := core.Session{Key: "claude:c1", Agent: model.Claude, SessionID: "c1", Cwd: dir, DefaultProfile: true, Title: "Old"}
	if err := a.moveSession(closed, "docs"); err != nil {
		t.Fatal(err)
	}
	out, err := a.tmux.Run("list-panes", "-s", "-t", "=docs", "-F", "#{window_active} #{pane_start_command}")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "claude --resume c1") || !strings.HasPrefix(out, "1 ") {
		t.Fatalf("panes:\n%s(want the resume in a new, not current, window)", out)
	}
	// A closed conversation whose directory is gone is not opened.
	gone := closed
	gone.Cwd = filepath.Join(dir, "deleted")
	if err := a.moveSession(gone, "docs"); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("%v", err)
	}
}
