// Package daemon is the imperative shell around the machine: it turns OS
// events (file changes, process exits, socket requests, the timer) into
// machine inputs, and executes the machine's effects through ports.
//
// It contains no policy. Every decision (what to notify, what to write to
// tmux, when to save, what to summarise) is made by package machine; the
// only judgement here is which sources an event makes stale (see Scope).
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"cccp/internal/claudereg"
	"cccp/internal/config"
	"cccp/internal/core"
	"cccp/internal/ipc"
	"cccp/internal/machine"
	"cccp/internal/store"
)

// Observer is the read side of the world (implemented by gather.Gatherer).
type Observer interface {
	Reload()
	HookFileChanged(path string) (rediscover bool)
	RegistryFileChanged(path string) (rediscover bool)
	RefreshTmux()
	Discover()
	Observe(now time.Time) core.Observation
}

// TmuxWriter sets cccp's tmux options.
type TmuxWriter interface {
	SetStatus(value string) error
	SetGlyph(window, value string) error
}

// Notifier shows a desktop notification.
type Notifier interface {
	Notify(argv []string) error
}

// Summarizer produces a one-line summary.
type Summarizer interface {
	Summarize(ctx context.Context, model, input string) (string, error)
}

// MemoryStore persists the machine's memory.
type MemoryStore interface {
	Save(core.Memory) error
}

// FileRemover deletes expired state files.
type FileRemover interface {
	Remove(path string) error
}

// Ports are the daemon's dependencies on the world.
type Ports struct {
	Observer   Observer
	Tmux       TmuxWriter
	Notifier   Notifier
	Summarizer Summarizer
	Memory     MemoryStore
	Files      FileRemover
	// LoadConfig rereads the configuration (on SIGHUP or a reload request).
	LoadConfig func() (config.Config, error)
	Now        func() time.Time
	Log        *log.Logger
}

// Options are the daemon's locations.
type Options struct {
	SessionsDir string // hook state files (watched)
	RegistryDir string // Claude registry (watched if it exists)
	Socket      string
	// SummaryTimeout bounds one summary.
	SummaryTimeout time.Duration
}

// Scope says whether the world must be observed and which sources an event
// made stale.
type Scope struct {
	Observe  bool // something relevant changed
	Reload   bool // reread every hook and registry file
	Tmux     bool // reread panes and clients
	Discover bool // rescan processes (implies Tmux)
}

func (s *Scope) add(o Scope) {
	s.Observe = s.Observe || o.Observe
	s.Reload = s.Reload || o.Reload
	s.Tmux = s.Tmux || o.Tmux
	s.Discover = s.Discover || o.Discover
}

var (
	scopeAll      = Scope{Observe: true, Reload: true, Tmux: true, Discover: true}
	scopeDiscover = Scope{Observe: true, Tmux: true, Discover: true}
	scopeTmux     = Scope{Observe: true, Tmux: true}
	scopeSources  = Scope{Observe: true} // the changed file is already reread
)

// TmuxEventScope maps a tmux hook name to what it makes stale: layout
// changes can add or remove agent processes; focus changes only change what
// is visible.
func TmuxEventScope(event string) Scope {
	switch event {
	case "pane-exited", "after-new-window", "after-split-window", "session-created", "session-closed", "client-attached":
		return scopeDiscover
	}
	return scopeTmux
}

// Daemon is a running daemon.
type Daemon struct {
	opt   Options
	ports Ports
	m     *machine.Machine

	reqs chan request
	pids chan int
	sums chan machine.SummaryDone

	subs    map[*subscriber]bool
	replies []chan ipc.Response // waiting for the next published snapshot
	pidw    *pidWatcher
	timer   *time.Timer
	ctx     context.Context
}

type request struct {
	req   ipc.Request
	reply chan ipc.Response // nil when no reply is expected
	sub   *subscriber
}

type subscriber struct {
	ch chan core.Snapshot // capacity 1: the latest snapshot wins
}

// New creates a daemon around machine m.
func New(opt Options, ports Ports, m *machine.Machine) *Daemon {
	if opt.SummaryTimeout == 0 {
		opt.SummaryTimeout = 60 * time.Second
	}
	return &Daemon{
		opt: opt, ports: ports, m: m,
		reqs: make(chan request, 64),
		pids: make(chan int, 64),
		sums: make(chan machine.SummaryDone, 4),
		subs: map[*subscriber]bool{},
	}
}

// Run serves until ctx is cancelled; reload delivers configuration reload
// requests (may be nil).
func (d *Daemon) Run(ctx context.Context, reload <-chan struct{}) error {
	d.ctx = ctx
	if c, err := net.DialTimeout("unix", d.opt.Socket, 200*time.Millisecond); err == nil {
		c.Close()
		return fmt.Errorf("another daemon is listening on %s", d.opt.Socket)
	}
	_ = os.Remove(d.opt.Socket)
	if err := os.MkdirAll(d.opt.SessionsDir, 0o700); err != nil {
		return err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if err := w.Add(d.opt.SessionsDir); err != nil {
		return err
	}
	if err := w.Add(d.opt.RegistryDir); err != nil {
		d.ports.Log.Printf("not watching the Claude registry: %v", err)
	}
	if d.pidw, err = newPidWatcher(d.pids); err != nil {
		return err
	}
	defer d.pidw.Close()
	ln, err := net.Listen("unix", d.opt.Socket)
	if err != nil {
		return err
	}
	_ = os.Chmod(d.opt.Socket, 0o600)
	defer func() {
		ln.Close()
		os.Remove(d.opt.Socket)
	}()
	go d.accept(ln)

	d.timer = time.NewTimer(time.Hour)
	d.timer.Stop()
	defer d.timer.Stop()

	d.observe(scopeAll)
	if snap, ok := d.m.Snapshot(); ok {
		d.ports.Log.Printf("cccp daemon started: %d sessions, socket %s", len(snap.Sessions), d.opt.Socket)
	}

	for {
		select {
		case <-ctx.Done():
			d.exec(d.m.Step(machine.Stop{At: d.ports.Now()}))
			d.ports.Log.Printf("cccp daemon stopped")
			return nil
		case <-reload:
			d.reloadConfig()
		case ev, ok := <-w.Events:
			if !ok {
				return errors.New("file watcher closed")
			}
			var sc Scope
			d.fileEvent(ev, &sc)
			d.drainAndObserve(w, sc)
		case err := <-w.Errors:
			// E.g. an inotify queue overflow: resynchronise from disk.
			d.ports.Log.Printf("watcher: %v", err)
			d.drainAndObserve(w, scopeAll)
		case <-d.pids:
			d.drainAndObserve(w, scopeDiscover)
		case r := <-d.reqs:
			var sc Scope
			d.request(r, &sc)
			d.drainAndObserve(w, sc)
		case done := <-d.sums:
			d.exec(d.m.Step(done))
		case <-d.timer.C:
			d.exec(d.m.Step(machine.TimerFired{At: d.ports.Now()}))
		}
	}
}

// drainAndObserve applies already-queued events, so one observation covers
// a burst, then observes if anything relevant happened.
func (d *Daemon) drainAndObserve(w *fsnotify.Watcher, sc Scope) {
drain:
	for i := 0; i < 256; i++ {
		select {
		case ev := <-w.Events:
			d.fileEvent(ev, &sc)
		case <-d.pids:
			sc.add(scopeDiscover)
		case r := <-d.reqs:
			d.request(r, &sc)
		default:
			break drain
		}
	}
	if sc.Observe {
		d.observe(sc)
	}
}

// fileEvent applies one watched-file change to the observer.
func (d *Daemon) fileEvent(ev fsnotify.Event, sc *Scope) {
	name := filepath.Base(ev.Name)
	switch filepath.Dir(ev.Name) {
	case filepath.Clean(d.opt.SessionsDir):
		if !store.IsStateFile(name) {
			return // lock and temp files
		}
		sc.add(scopeSources)
		if d.ports.Observer.HookFileChanged(ev.Name) {
			sc.add(scopeDiscover)
		}
	case filepath.Clean(d.opt.RegistryDir):
		if !claudereg.IsEntryFile(name) {
			return
		}
		sc.add(scopeSources)
		if d.ports.Observer.RegistryFileChanged(ev.Name) {
			sc.add(scopeDiscover)
		}
	}
}

// observe refreshes the stale sources and steps the machine.
func (d *Daemon) observe(sc Scope) {
	o := d.ports.Observer
	if sc.Reload {
		o.Reload()
	}
	if sc.Tmux || sc.Discover {
		o.RefreshTmux()
	}
	if sc.Discover {
		o.Discover()
	}
	now := d.ports.Now()
	d.exec(d.m.Step(machine.Observed{Obs: o.Observe(now), At: now}))
}

// request handles one socket request. Refreshes and tmux events widen sc
// (and are answered after the observation); the rest are answered here.
func (d *Daemon) request(r request, sc *Scope) {
	now := d.ports.Now()
	switch r.req.Op {
	case ipc.OpSubscribe:
		d.subs[r.sub] = true
		if snap, ok := d.m.Snapshot(); ok {
			offer(r.sub, snap)
		}
	case opUnsubscribe:
		delete(d.subs, r.sub)
	case ipc.OpSnapshot:
		if snap, ok := d.m.Snapshot(); ok {
			reply(r.reply, ipc.Response{OK: true, Snapshot: &snap})
		} else {
			d.replies = append(d.replies, r.reply)
		}
	case ipc.OpRefresh:
		sc.add(scopeAll)
		d.replies = append(d.replies, r.reply)
	case ipc.OpTmux:
		sc.add(TmuxEventScope(r.req.Event))
	case ipc.OpSeen:
		d.exec(d.m.Step(machine.MarkSeen{Key: r.req.Key, At: now}))
		reply(r.reply, ipc.Response{OK: true})
	case ipc.OpAlias:
		d.exec(d.m.Step(machine.SetAlias{Key: r.req.Key, Value: r.req.Value, At: now}))
		reply(r.reply, ipc.Response{OK: true})
	case ipc.OpReload:
		d.reloadConfig()
		reply(r.reply, ipc.Response{OK: true})
	default:
		reply(r.reply, ipc.Response{Error: "unknown op " + r.req.Op})
	}
}

func (d *Daemon) reloadConfig() {
	cfg, err := d.ports.LoadConfig()
	if err != nil {
		d.ports.Log.Printf("config: %v (keeping the current configuration)", err)
		return
	}
	d.exec(d.m.Step(machine.ConfigChanged{Config: cfg, At: d.ports.Now()}))
	d.ports.Log.Printf("config reloaded")
}

// exec carries out the machine's effects, in order.
func (d *Daemon) exec(effs []machine.Effect) {
	tmuxFailed := false
	for _, e := range effs {
		switch e := e.(type) {
		case machine.Publish:
			d.publish(e.Snapshot)
		case machine.SetStatus:
			if err := d.ports.Tmux.SetStatus(e.Value); err != nil {
				tmuxFailed = true
			}
		case machine.SetGlyph:
			if err := d.ports.Tmux.SetGlyph(e.Window, e.Value); err != nil {
				tmuxFailed = true
			}
		case machine.Notify:
			if err := d.ports.Notifier.Notify(e.Argv); err != nil {
				d.ports.Log.Printf("notify %s: %v", e.Key, err)
			}
		case machine.StartSummary:
			go d.summarize(e)
		case machine.SaveMemory:
			if err := d.ports.Memory.Save(e.Memory); err != nil {
				d.ports.Log.Printf("saving memory: %v", err)
			}
		case machine.RemoveFiles:
			for _, p := range e.Paths {
				if err := d.ports.Files.Remove(p); err != nil {
					d.ports.Log.Printf("removing %s: %v", p, err)
				}
			}
		case machine.ArmTimer:
			d.arm(e.At)
		case machine.Log:
			d.ports.Log.Print(e.Msg)
		default:
			d.ports.Log.Printf("daemon: unknown effect %T", e)
		}
	}
	if tmuxFailed {
		d.ports.Log.Printf("tmux: setting options failed; they will be re-sent")
		d.m.Step(machine.TmuxFailed{})
	}
}

func (d *Daemon) publish(snap core.Snapshot) {
	for s := range d.subs {
		offer(s, snap)
	}
	for _, ch := range d.replies {
		reply(ch, ipc.Response{OK: true, Snapshot: &snap})
	}
	d.replies = nil
	for _, s := range snap.Sessions {
		if s.Alive && s.PID > 0 {
			d.pidw.Watch(s.PID)
		}
	}
}

func (d *Daemon) arm(at time.Time) {
	if !d.timer.Stop() {
		select {
		case <-d.timer.C:
		default:
		}
	}
	if !at.IsZero() {
		// +50ms so the snapshot computed when it fires is past the deadline.
		d.timer.Reset(time.Until(at) + 50*time.Millisecond)
	}
}

// summarize runs one summary and reports back (unless shutting down).
func (d *Daemon) summarize(e machine.StartSummary) {
	ctx, cancel := context.WithTimeout(d.ctx, d.opt.SummaryTimeout)
	defer cancel()
	text, err := d.ports.Summarizer.Summarize(ctx, e.Model, e.Input)
	done := machine.SummaryDone{Key: e.Key, Hash: e.Hash, Text: text, Err: err, At: d.ports.Now()}
	select {
	case d.sums <- done:
	case <-d.ctx.Done():
	}
}

// ---- socket -------------------------------------------------------------------

const opUnsubscribe = "unsubscribe"

func reply(ch chan ipc.Response, r ipc.Response) {
	if ch != nil {
		ch <- r
	}
}

func offer(s *subscriber, snap core.Snapshot) {
	select {
	case <-s.ch:
	default:
	}
	s.ch <- snap
}

// send delivers a request to the loop unless the daemon is stopping.
func (d *Daemon) send(r request) bool {
	select {
	case d.reqs <- r:
		return true
	case <-d.ctx.Done():
		return false
	}
}

func (d *Daemon) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			d.ports.Log.Printf("accept: %v", err)
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
			d.send(request{req: req})
		case ipc.OpSubscribe:
			sub := &subscriber{ch: make(chan core.Snapshot, 1)}
			if d.send(request{req: req, sub: sub}) {
				d.stream(c, enc, sub)
			}
			return
		default:
			ch := make(chan ipc.Response, 1)
			if !d.send(request{req: req, reply: ch}) {
				return
			}
			select {
			case resp := <-ch:
				if err := enc.Encode(resp); err != nil {
					return
				}
			case <-d.ctx.Done():
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
	defer d.send(request{req: ipc.Request{Op: opUnsubscribe}, sub: sub})
	for {
		select {
		case snap := <-sub.ch:
			s := snap
			if err := enc.Encode(ipc.Response{OK: true, Snapshot: &s}); err != nil {
				return
			}
		case <-gone:
			return
		case <-d.ctx.Done():
			return
		}
	}
}
