package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"cccp/internal/agydb"
	"cccp/internal/catalog"
	"cccp/internal/claudereg"
	"cccp/internal/config"
	"cccp/internal/core"
	"cccp/internal/gather"
	"cccp/internal/ipc"
	"cccp/internal/memfile"
	"cccp/internal/model"
	"cccp/internal/paths"
	"cccp/internal/power"
	"cccp/internal/procs"
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

func (a *app) memoryFile() memfile.File {
	return memfile.File{Path: a.l.MemoryFile, SummariesPath: a.l.SummariesFile, WorkstreamsPath: a.l.WorkstreamsFile}
}

func (a *app) memory() core.Memory {
	m, err := a.memoryFile().Load()
	if err != nil {
		a.warn("%v (starting with empty memory)", err)
	}
	return m
}

// catalog is the conversation catalog and text index over the given
// profiles' transcripts (and agy's).
func (a *app) catalog(profiles []model.Profile) catalog.Index {
	ix := catalog.Index{TextDir: a.l.TextDir, AgyBrainDir: a.l.AgyBrainDir}
	for _, p := range profiles {
		ix.ClaudeConfigDirs = append(ix.ClaudeConfigDirs, p.Dir)
	}
	return ix
}

// indexer adapts the catalog to the daemon's Indexer port: the profiles
// are the daemon's current ones at each run.
type indexer struct{ base catalog.Index }

func (ix indexer) Update(configDirs []string) error {
	c := ix.base
	c.ClaudeConfigDirs = configDirs
	_, err := c.Update()
	return err
}

// updateCatalog brings the catalog up to date for an in-process snapshot
// (no daemon to do it) and rereads it into g. A failure is a warning: the
// live sessions do not depend on it.
func (a *app) updateCatalog(g *gather.Gatherer) {
	if _, err := a.catalog(g.Profiles()).Update(); err != nil {
		a.warn("conversation catalog: %v", err)
	}
	g.RefreshCatalog()
}

// gatherer wires the real sources.
func (a *app) gatherer(cfg config.Config) *gather.Gatherer {
	host, _ := os.Hostname()
	return gather.New(gather.Deps{
		Hooks:            a.store,
		Registry:         claudereg.Reader{},
		Tmux:             a.tmux,
		Procs:            procs.System{},
		Transcripts:      transcript.NewReader(),
		Files:            gather.DiskFiles{Home: a.l.Home, AgyBrainDir: a.l.AgyBrainDir},
		Catalog:          a.catalog(nil), // reading the catalog needs no transcript directories
		AgyDB:            agydb.NewCache(a.l.AgySummariesDB),
		Power:            power.System{},
		Hostname:         host,
		Home:             a.l.Home,
		DefaultClaudeDir: a.l.DefaultClaudeDir,
		Configured:       cfg.Profiles,
	})
}

// profiles resolves every Claude profile: configured, and discovered from
// ~/.claude* directories, hook state and running sessions.
func (a *app) profiles(cfg config.Config) []model.Profile {
	g := a.gatherer(cfg)
	g.Reload()
	g.RefreshTmux()
	g.Discover()
	return g.Profiles()
}

// localSnapshot computes a snapshot in-process (no daemon). Memory is read,
// never written: seen marks are the daemon's to keep.
func (a *app) localSnapshot() core.Snapshot {
	cfg := a.config()
	g := a.gatherer(cfg)
	g.Reload()
	g.RefreshTmux()
	g.Discover()
	a.updateCatalog(g)
	now := time.Now()
	snap, _ := core.Compute(g.Observe(now), a.memory(), cfg, now, now)
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
