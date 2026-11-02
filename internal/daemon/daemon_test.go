package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
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
