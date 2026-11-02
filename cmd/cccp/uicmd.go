package main

import (
	"errors"
	"flag"
	"sync"
	"time"

	"cccp/internal/engine"
	"cccp/internal/ipc"
	"cccp/internal/tmux"
	"cccp/internal/ui"
)

func runUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	client := fs.String("client", "", "tmux client to switch when jumping (default: current)")
	_ = fs.Parse(args)
	tm := tmux.Default()
	if *client == "" {
		*client = currentClient(tm)
	}
	var src ui.Source
	if sub, err := ipc.Subscribe(time.Second); err == nil {
		src = &daemonSource{sub: sub}
	} else {
		e, err := localEngine()
		if err != nil {
			return err
		}
		src = &localSource{eng: e}
	}
	daemon := src.Daemon()
	return ui.Run(ui.Options{
		Client: *client,
		Source: src,
		Tmux:   tm,
		Actions: ui.Actions{
			Jump: func(s engine.Session) error { return jumpTo(tm, *client, s) },
			Alias: func(key, value string) error {
				if !daemon {
					return errors.New("aliases need the daemon (systemctl --user start cccp)")
				}
				_, err := ipc.Call(ipc.Request{Op: ipc.OpAlias, Key: key, Value: value}, 2*time.Second)
				return err
			},
			Seen: func(key string) error {
				if !daemon {
					return errors.New("seen marks need the daemon")
				}
				_, err := ipc.Call(ipc.Request{Op: ipc.OpSeen, Key: key}, 2*time.Second)
				return err
			},
		},
	})
}

// daemonSource streams snapshots from the daemon.
type daemonSource struct {
	sub *ipc.Subscription
}

func (d *daemonSource) Start(send func(engine.Snapshot)) {
	go func() {
		for {
			s, err := d.sub.Next()
			if err != nil {
				return
			}
			send(s)
		}
	}()
}

func (d *daemonSource) Refresh() {
	go func() { _, _ = ipc.Call(ipc.Request{Op: ipc.OpRefresh}, 5*time.Second) }()
}

func (d *daemonSource) Daemon() bool { return true }
func (d *daemonSource) Close()       { d.sub.Close() }

// localSource runs the engine in-process while the cockpit is open. Without
// a daemon there are no change events, so it rescans every two seconds; this
// only happens while the cockpit is on screen.
type localSource struct {
	eng  *engine.Engine
	mu   sync.Mutex
	stop chan struct{}
	kick chan struct{}
}

func (l *localSource) Start(send func(engine.Snapshot)) {
	l.stop, l.kick = make(chan struct{}), make(chan struct{}, 1)
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			l.mu.Lock()
			s := l.eng.Snapshot(time.Now())
			l.mu.Unlock()
			send(s)
			select {
			case <-l.stop:
				return
			case <-t.C:
				l.mu.Lock()
				l.eng.LoadHooks()
				l.eng.LoadRegistry()
				l.eng.RefreshTmux()
				l.mu.Unlock()
			case <-l.kick:
				l.mu.Lock()
				l.eng.LoadAll()
				l.mu.Unlock()
			}
		}
	}()
}

func (l *localSource) Refresh() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

func (l *localSource) Daemon() bool { return false }
func (l *localSource) Close()       { close(l.stop) }
