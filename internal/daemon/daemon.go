// Package daemon runs cccpd: it watches every source, keeps the engine's
// snapshot current, decorates tmux, notifies, summarises, and serves the
// snapshot over a unix socket.
//
// One goroutine (run) owns the engine and all state; everything else only
// feeds it events. Idle, every goroutine is blocked in the kernel.
package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"cccp/internal/claudereg"
	"cccp/internal/config"
	"cccp/internal/engine"
	"cccp/internal/ipc"
	"cccp/internal/paths"
	"cccp/internal/power"
	"cccp/internal/present"
	"cccp/internal/store"
)

type request struct {
	req   ipc.Request
	reply chan ipc.Response // nil for fire-and-forget
	sub   *subscriber       // set for OpSubscribe
}

type subscriber struct {
	ch chan engine.Snapshot // capacity 1: the latest snapshot wins
}

// Daemon is the running daemon.
type Daemon struct {
	eng  *engine.Engine
	log  *log.Logger
	reqs chan request
	pids chan int
	sums chan summaryResult

	watcher *fsnotify.Watcher
	pidw    *pidWatcher

	subs map[*subscriber]bool
	last engine.Snapshot
	have bool

	// tmux decoration state, to set options only when values change
	appliedStatus string
	appliedWin    map[string]string
	tmuxWasOK     bool

	notified map[string]string // key → signature of the last notified state

	sumBusy  bool
	sumQueue []summaryJob

	timer     *time.Timer
	saveDue   time.Time
	lastPrune time.Time

	// pendingReply collects replies to send with the next snapshot.
	pendingReply []chan ipc.Response
	// tmuxOnly: re-read tmux (but skip the process scan) on the next
	// recompute.
	tmuxOnly bool
}

// Run starts the daemon and blocks until SIGINT/SIGTERM or until stop is
// closed (stop may be nil).
func Run(logger *log.Logger, stop <-chan struct{}) error {
	if c, err := net.DialTimeout("unix", paths.Socket(), 200*time.Millisecond); err == nil {
		c.Close()
		return fmt.Errorf("another daemon is listening on %s", paths.Socket())
	}
	_ = os.Remove(paths.Socket())

	cfg, err := config.Load()
	if err != nil {
		logger.Printf("config: %v (using defaults)", err)
	}
	persist, err := engine.LoadPersist()
	if err != nil {
		logger.Printf("daemon state: %v (starting fresh)", err)
	}
	d := &Daemon{
		eng:        engine.New(cfg, persist),
		log:        logger,
		reqs:       make(chan request, 64),
		pids:       make(chan int, 64),
		sums:       make(chan summaryResult, 4),
		subs:       map[*subscriber]bool{},
		appliedWin: map[string]string{},
		notified:   map[string]string{},
		timer:      time.NewTimer(time.Hour),
	}
	d.timer.Stop()

	if err := os.MkdirAll(paths.SessionsDir(), 0o700); err != nil {
		return err
	}
	d.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer d.watcher.Close()
	if err := d.watcher.Add(paths.SessionsDir()); err != nil {
		return err
	}
	if err := d.watcher.Add(paths.ClaudeRegistryDir()); err != nil {
		logger.Printf("not watching the Claude registry: %v", err)
	}
	if d.pidw, err = newPidWatcher(d.pids); err != nil {
		return err
	}

	ln, err := net.Listen("unix", paths.Socket())
	if err != nil {
		return err
	}
	_ = os.Chmod(paths.Socket(), 0o600)
	defer func() {
		ln.Close()
		os.Remove(paths.Socket())
	}()
	go d.accept(ln)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	d.eng.LoadAll()
	d.recompute(true)
	d.prune()
	logger.Printf("cccp daemon started: %d sessions, socket %s", len(d.last.Sessions), paths.Socket())

	for {
		select {
		case <-stop:
			d.shutdown()
			return nil
		case s := <-sig:
			if s == syscall.SIGHUP {
				d.reloadConfig()
				continue
			}
			d.shutdown()
			return nil
		case ev, ok := <-d.watcher.Events:
			if !ok {
				return errors.New("file watcher closed")
			}
			full := d.handleFS(ev)
			d.drain(&full)
			d.recompute(full)
		case err := <-d.watcher.Errors:
			logger.Printf("watcher: %v", err)
			// Overflow or similar: resynchronise from disk.
			d.eng.LoadHooks()
			d.eng.LoadRegistry()
			d.recompute(true)
		case <-d.pids:
			full := true
			d.drain(&full)
			d.recompute(true)
		case r := <-d.reqs:
			full := d.handleReq(r)
			d.drain(&full)
			d.recompute(full)
		case res := <-d.sums:
			d.handleSummary(res)
			d.recompute(false)
		case <-d.timer.C:
			d.recompute(false)
		}
	}
}

// drain applies already-queued events so one recompute covers a burst.
func (d *Daemon) drain(full *bool) {
	for i := 0; i < 256; i++ {
		select {
		case ev := <-d.watcher.Events:
			if d.handleFS(ev) {
				*full = true
			}
		case <-d.pids:
			*full = true
		case r := <-d.reqs:
			if d.handleReq(r) {
				*full = true
			}
		default:
			return
		}
	}
}

// handleFS applies one file event; it returns true when tmux/proc
// rediscovery is needed (a session appeared or disappeared).
func (d *Daemon) handleFS(ev fsnotify.Event) bool {
	dir, name := filepath.Dir(ev.Name), filepath.Base(ev.Name)
	switch dir {
	case paths.SessionsDir():
		if !store.IsStateFile(name) {
			return false
		}
		before, had := d.eng.HookFile(ev.Name)
		d.eng.LoadHookFile(ev.Name)
		after, has := d.eng.HookFile(ev.Name)
		// Rediscover when a session appears or goes away, or moves to
		// another process or pane (resume, restart).
		return had != has || before.PID != after.PID || before.Pane != after.Pane
	case paths.ClaudeRegistryDir():
		if !claudereg.IsEntryFile(name) {
			return false
		}
		before, had := d.eng.RegistryFile(ev.Name)
		d.eng.LoadRegistryFile(ev.Name)
		after, has := d.eng.RegistryFile(ev.Name)
		return had != has || before.SessionID != after.SessionID || before.Tmux != after.Tmux
	}
	return false
}

// handleReq applies one request; it returns true when rediscovery is needed.
func (d *Daemon) handleReq(r request) bool {
	switch r.req.Op {
	case ipc.OpSubscribe:
		d.subs[r.sub] = true
		if d.have {
			offer(r.sub, d.last)
		}
		return false
	case "unsubscribe":
		delete(d.subs, r.sub)
		return false
	case ipc.OpSnapshot:
		d.pendingReply = append(d.pendingReply, r.reply)
		return false
	case ipc.OpRefresh:
		d.eng.LoadHooks()
		d.eng.LoadRegistry()
		d.pendingReply = append(d.pendingReply, r.reply)
		return true
	case ipc.OpTmux:
		switch r.req.Event {
		case "pane-exited", "after-new-window", "after-split-window", "session-created", "session-closed", "client-attached":
			return true
		}
		d.tmuxOnly = true
		return false
	case ipc.OpSeen:
		d.eng.Persist.Seen[r.req.Key] = time.Now()
		d.eng.PersistDirty = true
		reply(r.reply, ipc.Response{OK: true})
	case ipc.OpAlias:
		if v := strings.TrimSpace(r.req.Value); v == "" {
			delete(d.eng.Persist.Aliases, r.req.Key)
		} else {
			d.eng.Persist.Aliases[r.req.Key] = v
		}
		d.eng.PersistDirty = true
		reply(r.reply, ipc.Response{OK: true})
	case ipc.OpReload:
		d.reloadConfig()
		reply(r.reply, ipc.Response{OK: true})
	default:
		reply(r.reply, ipc.Response{Error: "unknown op " + r.req.Op})
	}
	return false
}

func reply(ch chan ipc.Response, r ipc.Response) {
	if ch != nil {
		ch <- r
	}
}

func offer(s *subscriber, snap engine.Snapshot) {
	select {
	case <-s.ch:
	default:
	}
	s.ch <- snap
}

func (d *Daemon) reloadConfig() {
	cfg, err := config.Load()
	if err != nil {
		d.log.Printf("config: %v (keeping current)", err)
		return
	}
	d.eng.SetConfig(cfg)
	d.log.Printf("config reloaded")
}

// recompute refreshes the snapshot and applies side effects. full also
// re-reads tmux and rescans processes.
func (d *Daemon) recompute(full bool) {
	if full || d.tmuxOnly {
		d.eng.RefreshTmux()
	}
	if full {
		d.eng.Discover()
	}
	d.tmuxOnly = false
	now := time.Now()
	snap := d.eng.Snapshot(now)
	snap.FromDaemon = true
	prev, hadPrev := d.last, d.have
	d.last, d.have = snap, true

	for _, s := range snap.Sessions {
		if s.Alive && s.PID > 0 {
			d.pidw.Watch(s.PID)
		}
	}
	d.decorateTmux(snap)
	if hadPrev {
		d.notify(prev, snap)
	} else {
		d.primeNotified(snap)
	}
	d.queueSummaries(snap)
	for s := range d.subs {
		offer(s, snap)
	}
	for _, ch := range d.pendingReply {
		reply(ch, ipc.Response{OK: true, Snapshot: &snap})
	}
	d.pendingReply = nil

	if d.eng.PersistDirty && d.saveDue.IsZero() {
		d.saveDue = now.Add(5 * time.Second)
	}
	if !d.saveDue.IsZero() && !now.Before(d.saveDue) {
		d.save()
	}
	if now.Sub(d.lastPrune) > 6*time.Hour {
		d.prune()
	}
	d.arm(snap, now)
}

func (d *Daemon) save() {
	if err := d.eng.Persist.Save(); err != nil {
		d.log.Printf("saving daemon state: %v", err)
	}
	d.eng.PersistDirty = false
	d.saveDue = time.Time{}
}

func (d *Daemon) prune() {
	d.lastPrune = time.Now()
	if n := d.eng.PruneEnded(d.last); n > 0 {
		d.log.Printf("pruned %d ended sessions", n)
	}
}

// arm sets the single timer to the earliest deadline: a cache expiring or a
// pending state save.
func (d *Daemon) arm(snap engine.Snapshot, now time.Time) {
	t := engine.NextDeadline(snap)
	if !d.saveDue.IsZero() && (t.IsZero() || d.saveDue.Before(t)) {
		t = d.saveDue
	}
	if !d.timer.Stop() {
		select {
		case <-d.timer.C:
		default:
		}
	}
	if !t.IsZero() {
		// +50ms so the snapshot computed at fire time is past the deadline.
		d.timer.Reset(t.Sub(now) + 50*time.Millisecond)
	}
}

func (d *Daemon) shutdown() {
	d.save()
	tm := d.eng.Tmux()
	_ = tm.SetGlobalOption("@cccp_status", "")
	for w := range d.appliedWin {
		_ = tm.SetWindowOption(w, "@cccp_glyph", "")
	}
	d.log.Printf("cccp daemon stopped")
}

// decorateTmux writes @cccp_status and per-window @cccp_glyph, only for
// values that changed.
func (d *Daemon) decorateTmux(snap engine.Snapshot) {
	if !snap.TmuxOK {
		d.tmuxWasOK = false
		return
	}
	if !d.tmuxWasOK {
		// A (re)started tmux server has none of our options.
		d.appliedStatus = "\x00"
		d.appliedWin = map[string]string{}
		d.tmuxWasOK = true
	}
	tm := d.eng.Tmux()
	status := present.StatusSegment(snap)
	if status != d.appliedStatus {
		if err := tm.SetGlobalOption("@cccp_status", status); err != nil {
			d.log.Printf("tmux: %v", err)
		} else {
			d.appliedStatus = status
		}
	}
	want := present.WindowGlyphs(snap)
	for w, g := range want {
		if d.appliedWin[w] != g {
			if err := tm.SetWindowOption(w, "@cccp_glyph", g); err == nil {
				d.appliedWin[w] = g
			}
		}
	}
	for w := range d.appliedWin {
		if _, ok := want[w]; !ok {
			_ = tm.SetWindowOption(w, "@cccp_glyph", "")
			delete(d.appliedWin, w)
		}
	}
}

// signature identifies an attention state for notification de-duplication.
func signature(s engine.Session) string {
	if !s.NeedsAttention() || !s.Alive {
		return ""
	}
	return fmt.Sprintf("%d|%s|%d", s.Group, s.Activity, s.ActivitySince.UnixMilli())
}

func (d *Daemon) primeNotified(snap engine.Snapshot) {
	for _, s := range snap.Sessions {
		if sig := signature(s); sig != "" {
			d.notified[s.Key] = sig
		}
	}
}

// notify sends a desktop notification for sessions that newly need
// attention while their pane is not visible.
func (d *Daemon) notify(prev, snap engine.Snapshot) {
	cfg := d.eng.Config()
	for _, s := range snap.Sessions {
		sig := signature(s)
		if sig == "" {
			delete(d.notified, s.Key)
			continue
		}
		if d.notified[s.Key] == sig {
			continue
		}
		d.notified[s.Key] = sig
		if !cfg.Notify || s.Visible || len(cfg.NotifyCommand) == 0 {
			continue
		}
		title := "cccp: " + s.Group.String() + " — " + s.DisplayTitle()
		body := present.Detail(s)
		if s.Inferred {
			body = "(inferred) " + body
		}
		if s.Location != "" {
			body = s.Location + "  " + body
		}
		argv := append(append([]string{}, cfg.NotifyCommand...), title, body)
		cmd := exec.Command(argv[0], argv[1:]...)
		if err := cmd.Start(); err != nil {
			d.log.Printf("notify: %v", err)
			continue
		}
		go cmd.Wait()
	}
}

// queueSummaries schedules one-line summaries for sessions that need you,
// when the configuration and power state allow it.
func (d *Daemon) queueSummaries(snap engine.Snapshot) {
	cfg := d.eng.Config()
	switch cfg.Summaries {
	case "off":
		return
	case "ac":
		if power.OnBattery() {
			return
		}
	}
	for _, s := range snap.Sessions {
		if !s.NeedsAttention() || !s.Alive || (s.LastMessage == "" && s.Question == "") {
			continue
		}
		in, hash := summaryInput(s)
		if rec, ok := d.eng.Persist.Summaries[s.Key]; ok && rec.InputHash == hash {
			continue
		}
		queued := false
		for _, j := range d.sumQueue {
			if j.key == s.Key && j.hash == hash {
				queued = true
			}
		}
		if !queued {
			d.sumQueue = append(d.sumQueue, summaryJob{key: s.Key, hash: hash, input: in})
		}
	}
	d.startSummary()
}

func (d *Daemon) startSummary() {
	if d.sumBusy || len(d.sumQueue) == 0 {
		return
	}
	job := d.sumQueue[0]
	d.sumQueue = d.sumQueue[1:]
	d.sumBusy = true
	model := d.eng.Config().SummaryModel
	go func() { d.sums <- runSummary(job, model) }()
}

func (d *Daemon) handleSummary(res summaryResult) {
	d.sumBusy = false
	if res.err != nil {
		d.log.Printf("summary for %s failed after %v: %v", res.key, res.took.Round(time.Millisecond), res.err)
	} else if res.rec.Text != "" {
		d.eng.Persist.Summaries[res.key] = res.rec
		d.eng.PersistDirty = true
	}
	d.startSummary()
}

// accept serves socket connections.
func (d *Daemon) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			d.log.Printf("accept: %v", err)
			continue
		}
		go d.serve(c)
	}
}

func (d *Daemon) serve(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	enc := json.NewEncoder(c)
	for sc.Scan() {
		var req ipc.Request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			_ = enc.Encode(ipc.Response{Error: "bad request: " + err.Error()})
			return
		}
		switch req.Op {
		case ipc.OpTmux:
			d.reqs <- request{req: req}
		case ipc.OpSubscribe:
			sub := &subscriber{ch: make(chan engine.Snapshot, 1)}
			d.reqs <- request{req: req, sub: sub}
			d.stream(c, enc, sub)
			return
		default:
			ch := make(chan ipc.Response, 1)
			d.reqs <- request{req: req, reply: ch}
			if err := enc.Encode(<-ch); err != nil {
				return
			}
		}
	}
}

// stream writes snapshots to a subscriber until it disconnects.
func (d *Daemon) stream(c net.Conn, enc *json.Encoder, sub *subscriber) {
	gone := make(chan struct{})
	go func() {
		// A subscriber sends nothing more; EOF means it went away.
		buf := make([]byte, 1)
		for {
			if _, err := c.Read(buf); err != nil {
				close(gone)
				return
			}
		}
	}()
	defer func() { d.reqs <- request{req: ipc.Request{Op: "unsubscribe"}, sub: sub} }()
	for {
		select {
		case snap := <-sub.ch:
			s := snap
			if err := enc.Encode(ipc.Response{OK: true, Snapshot: &s}); err != nil {
				return
			}
		case <-gone:
			return
		}
	}
}
