package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"cccp/internal/agydb"
	"cccp/internal/claudereg"
	"cccp/internal/config"
	"cccp/internal/core"
	"cccp/internal/gather"
	"cccp/internal/ipc"
	"cccp/internal/memfile"
	"cccp/internal/paths"
	"cccp/internal/power"
	"cccp/internal/procs"
	"cccp/internal/search"
	"cccp/internal/store"
	"cccp/internal/tmux"
	"cccp/internal/transcript"
)

// app is the composition root: the only place that reads the environment
// and constructs the real adapters. Everything below receives its
// dependencies from here.
type app struct {
	l      paths.Layout
	tmux   *tmux.Client
	daemon ipc.Client
	store  store.Store
	stderr io.Writer
}

func newApp() *app {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	l := paths.Resolve(os.Getenv, home, os.Getuid())
	return &app{
		l:      l,
		tmux:   tmux.New(l.TmuxSocket),
		daemon: ipc.Client{Socket: l.Socket},
		store:  store.Store{Dir: l.SessionsDir},
		stderr: os.Stderr,
	}
}

func (a *app) warn(format string, args ...any) {
	fmt.Fprintf(a.stderr, "cccp: "+format+"\n", args...)
}

// loadConfig reads the configuration file; a missing file is the defaults.
func (a *app) loadConfig() (config.Config, error) {
	b, err := os.ReadFile(a.l.ConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return config.Default(), nil
	}
	if err != nil {
		return config.Default(), err
	}
	c, err := config.Parse(b)
	if err != nil {
		return c, fmt.Errorf("%s: %w", a.l.ConfigFile, err)
	}
	return c, nil
}

// config loads the configuration, falling back to defaults with a warning.
func (a *app) config() config.Config {
	c, err := a.loadConfig()
	if err != nil {
		a.warn("%v (using defaults)", err)
	}
	return c
}

func (a *app) memoryFile() memfile.File { return memfile.File{Path: a.l.MemoryFile} }

func (a *app) memory() core.Memory {
	m, err := a.memoryFile().Load()
	if err != nil {
		a.warn("%v (starting with empty memory)", err)
	}
	return m
}

func (a *app) searchIndex() search.Index {
	return search.Index{TextDir: a.l.TextDir, ClaudeProjectsDir: a.l.ClaudeProjectsDir, AgyBrainDir: a.l.AgyBrainDir}
}

// gatherer wires the real sources.
func (a *app) gatherer() *gather.Gatherer {
	host, _ := os.Hostname()
	return gather.New(gather.Deps{
		Hooks:       a.store,
		Registry:    claudereg.Dir{Path: a.l.ClaudeRegistryDir},
		Tmux:        a.tmux,
		Procs:       procs.System{},
		Transcripts: transcript.NewReader(),
		Files: gather.DiskFiles{
			ClaudeProjectsDir: a.l.ClaudeProjectsDir,
			AgyBrainDir:       a.l.AgyBrainDir,
			ProjectDir:        a.l.ClaudeProjectDir,
		},
		AgyDB:    agydb.NewCache(a.l.AgySummariesDB),
		Power:    power.System{},
		Hostname: host,
	})
}

// localSnapshot computes a snapshot in-process (no daemon). Memory is read,
// never written: seen marks are the daemon's to keep.
func (a *app) localSnapshot() core.Snapshot {
	g := a.gatherer()
	g.Reload()
	g.RefreshTmux()
	g.Discover()
	now := time.Now()
	snap, _ := core.Compute(g.Observe(now), a.memory(), a.config(), now, now)
	return snap
}

// snapshot asks the daemon, falling back to an in-process computation.
func (a *app) snapshot(refresh bool) core.Snapshot {
	op := ipc.OpSnapshot
	if refresh {
		op = ipc.OpRefresh
	}
	if resp, err := a.daemon.Call(ipc.Request{Op: op}, 3*time.Second); err == nil && resp.Snapshot != nil {
		return *resp.Snapshot
	}
	return a.localSnapshot()
}
