package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cccp/internal/config"
	"cccp/internal/engine"
	"cccp/internal/hook"
	"cccp/internal/ipc"
	"cccp/internal/model"
	"cccp/internal/paths"
	"cccp/internal/present"
)

// runHook is the agents' hook entry point. Whatever happens it exits 0 and
// writes nothing a hook runner could read as a decision: for agy, which
// requires JSON on stdout, it prints "{}"; for Claude it prints nothing.
func runHook(args []string, start time.Time) int {
	agent := model.Agent("")
	if len(args) > 0 {
		agent = model.Agent(args[0])
	}
	defer func() {
		if r := recover(); r != nil {
			hookLog(fmt.Errorf("panic: %v", r))
		}
		if agent == model.Agy {
			fmt.Println("{}")
		}
	}()
	if err := hook.Run(agent, os.Stdin, start); err != nil {
		hookLog(err)
	}
	return 0
}

// hookLog appends hook errors to a log file (never to the agent's output).
func hookLog(err error) {
	_ = os.MkdirAll(paths.StateDir(), 0o700)
	f, ferr := os.OpenFile(filepath.Join(paths.StateDir(), "hook-errors.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %v\n", time.Now().Format(time.RFC3339), err)
}

// snapshot asks the daemon, falling back to an in-process engine.
func snapshot(refresh bool) (engine.Snapshot, error) {
	op := ipc.OpSnapshot
	if refresh {
		op = ipc.OpRefresh
	}
	if resp, err := ipc.Call(ipc.Request{Op: op}, 3*time.Second); err == nil && resp.Snapshot != nil {
		return *resp.Snapshot, nil
	}
	return localSnapshot()
}

func localEngine() (*engine.Engine, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	p, err := engine.LoadPersist()
	if err != nil {
		log.Printf("daemon state unreadable, starting fresh: %v", err)
	}
	e := engine.New(cfg, p)
	e.LoadAll()
	return e, nil
}

func localSnapshot() (engine.Snapshot, error) {
	e, err := localEngine()
	if err != nil {
		return engine.Snapshot{}, err
	}
	return e.Snapshot(time.Now()), nil
}

func runList(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the snapshot as JSON")
	all := fs.Bool("all", false, "include ended sessions")
	local := fs.Bool("local", false, "compute in-process even if the daemon is running")
	_ = fs.Parse(args)
	var snap engine.Snapshot
	var err error
	if *local {
		snap, err = localSnapshot()
	} else {
		snap, err = snapshot(false)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(snap)
	}
	printList(os.Stdout, snap, *all)
	return nil
}

func printList(w io.Writer, snap engine.Snapshot, all bool) {
	now := snap.At
	src := "in-process"
	if snap.FromDaemon {
		src = "daemon"
	}
	fmt.Fprintf(w, "%s   (%s%s)\n", present.PlainStatus(snap), src, map[bool]string{true: ", on battery"}[snap.OnBattery])
	for _, p := range snap.Problems {
		fmt.Fprintf(w, "! %s\n", p)
	}
	last := engine.Group(-1)
	for _, s := range snap.Sessions {
		if s.Group == engine.GroupEnded && !all {
			continue
		}
		if s.Group != last {
			fmt.Fprintf(w, "\n%s\n", s.Group)
			last = s.Group
		}
		inf := " "
		if s.Inferred {
			inf = "~"
		}
		loc := s.Location
		if loc == "" {
			loc = "-"
		}
		cache := present.CacheText(s, now)
		ctx := present.Tokens(s.ContextTokens)
		meta := strings.TrimSpace(strings.Join(nonEmpty(present.Since(now, s.ActivitySince), cache, ctx), "  "))
		fmt.Fprintf(w, " %s%s %s %-12s %-40s %s\n", present.Glyph(s), inf, present.AgentBadge(s.Agent), clipStr(loc, 12), clipStr(s.DisplayTitle(), 40), meta)
		if d := present.Detail(s); d != "" {
			fmt.Fprintf(w, "        %s\n", clipStr(d, 110))
		}
	}
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func clipStr(s string, n int) string {
	r := []rune(present.OneLine(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	tmuxStyle := fs.Bool("tmux", false, "include tmux style sequences")
	_ = fs.Parse(args)
	snap, err := snapshot(false)
	if err != nil {
		return err
	}
	if *tmuxStyle {
		fmt.Println(present.StatusSegment(snap))
	} else {
		fmt.Println(present.PlainStatus(snap))
	}
	return nil
}
