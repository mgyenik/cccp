package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cccp/internal/ipc"
	"cccp/internal/model"
	"cccp/internal/procs"
	"cccp/internal/store"
)

// TestDaemonPushesHookChanges runs the real daemon against temp dirs and
// checks that a hook state write reaches a subscriber through the file
// watcher and the socket.
func TestDaemonPushesHookChanges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CCCP_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("CCCP_CACHE_DIR", filepath.Join(dir, "cache"))
	t.Setenv("CCCP_SOCKET", filepath.Join(dir, "d.sock"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("HOME", dir)
	t.Setenv("CCCP_TMUX_SOCKET", filepath.Join(dir, "no-tmux"))
	if err := os.MkdirAll(filepath.Join(dir, "claude", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- Run(log.New(io.Discard, "", 0), stop) }()
	defer func() {
		close(stop)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	var sub *ipc.Subscription
	var err error
	for i := 0; i < 100; i++ {
		if sub, err = ipc.Subscribe(time.Second); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("daemon did not come up: %v", err)
	}
	defer sub.Close()
	if _, err := sub.Next(); err != nil { // initial snapshot
		t.Fatal(err)
	}

	st, _ := procs.ReadStat(os.Getpid())
	now := time.Now()
	if err := store.Update(model.Claude, "live", func(model.HookState) model.HookState {
		return model.HookState{Version: 1, Agent: model.Claude, SessionID: "live", PID: st.PID, PIDStart: st.StartTime,
			Activity: model.NeedsApproval, ActivityAt: now, LastEventAt: now, Question: "Allow Bash: make"}
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	got := make(chan bool, 1)
	go func() {
		for {
			snap, err := sub.Next()
			if err != nil {
				return
			}
			for _, s := range snap.Sessions {
				if s.Key == "claude:live" && s.Activity == model.NeedsApproval && s.Question == "Allow Bash: make" && snap.FromDaemon {
					got <- true
					return
				}
			}
		}
	}()
	select {
	case <-got:
	case <-deadline:
		t.Fatal("hook change did not reach the subscriber")
	}

	// The session needs approval and has no visible pane: one notification
	// (recorded by testNotifier, never shown).
	deadlineN := time.Now().Add(2 * time.Second)
	for len(sentNotifications()) == 0 && time.Now().Before(deadlineN) {
		time.Sleep(10 * time.Millisecond)
	}
	ns := sentNotifications()
	if len(ns) != 1 {
		t.Fatalf("want exactly 1 notification, got %d: %q", len(ns), ns)
	}
	if got := ns[0]; got[0] != "notify-send" || !strings.Contains(got[len(got)-2], "NEEDS YOU") || !strings.Contains(got[len(got)-1], "Allow Bash: make") {
		t.Fatalf("notification argv: %q", got)
	}

	// Request/response ops work too.
	if _, err := ipc.Call(ipc.Request{Op: ipc.OpAlias, Key: "claude:live", Value: "build"}, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	resp, err := ipc.Call(ipc.Request{Op: ipc.OpSnapshot}, 2*time.Second)
	if err != nil || resp.Snapshot == nil {
		t.Fatal(err)
	}
	for _, s := range resp.Snapshot.Sessions {
		if s.Key == "claude:live" && s.Alias != "build" {
			t.Fatalf("alias not applied: %+v", s)
		}
	}
}

// TestFailingSummariesDoNotLoop: a summarizer that always fails (logged out,
// usage limit) must be called once per input, not respawned on every
// recompute.
func TestFailingSummariesDoNotLoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CCCP_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("CCCP_CACHE_DIR", filepath.Join(dir, "cache"))
	t.Setenv("CCCP_SOCKET", filepath.Join(dir, "d.sock"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("HOME", dir)
	t.Setenv("CCCP_TMUX_SOCKET", filepath.Join(dir, "no-tmux"))
	os.MkdirAll(filepath.Join(dir, "claude", "sessions"), 0o700)
	os.MkdirAll(filepath.Join(dir, "config", "cccp"), 0o700)
	if err := os.WriteFile(filepath.Join(dir, "config", "cccp", "config.toml"), []byte("summaries = \"always\"\nnotify = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := make(chan struct{}, 100)
	old := summaryRunner
	summaryRunner = func(job summaryJob, model string) summaryResult {
		calls <- struct{}{}
		return summaryResult{key: job.key, err: errors.New("simulated: usage limit reached")}
	}
	defer func() { summaryRunner = old }()

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- Run(log.New(io.Discard, "", 0), stop) }()
	defer func() { close(stop); <-done }()
	for i := 0; i < 100; i++ {
		if _, err := ipc.Call(ipc.Request{Op: ipc.OpSnapshot}, time.Second); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	st, _ := procs.ReadStat(os.Getpid())
	// 20 events for a session that needs you, each forcing a recompute.
	for i := 0; i < 20; i++ {
		now := time.Now()
		if err := store.Update(model.Claude, "blocked", func(model.HookState) model.HookState {
			return model.HookState{Version: 1, Agent: model.Claude, SessionID: "blocked", PID: st.PID, PIDStart: st.StartTime,
				Activity: model.NeedsApproval, ActivityAt: now.Add(-time.Minute), LastEventAt: now,
				Question: "Allow Bash: make install", LastMessage: "Ready to install."}
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(calls); n != 1 {
		t.Fatalf("summarizer called %d times for one unchanged input; want 1", n)
	}
}
