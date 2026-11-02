package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cccp/internal/core"
	"cccp/internal/install"
	"cccp/internal/ipc"
)

// runDoctor checks each part of the installation and reports what it found.
func runDoctor(a *app, args []string) error {
	ok, bad := 0, 0
	check := func(name string, pass bool, detail string) {
		mark := "✓"
		if pass {
			ok++
		} else {
			mark = "✗"
			bad++
		}
		fmt.Printf("%s %-20s %s\n", mark, name, detail)
	}
	l := a.l

	if fi, err := os.Stat(l.BinPath); err == nil && fi.Mode()&0o111 != 0 {
		check("binary", true, l.BinPath)
	} else {
		check("binary", false, l.BinPath+" missing (hooks fall back to a silent no-op)")
	}

	resp, err := a.daemon.Call(ipc.Request{Op: ipc.OpSnapshot}, 2*time.Second)
	if err == nil && resp.Snapshot != nil {
		check("daemon", true, fmt.Sprintf("answering on %s (%d sessions)", l.Socket, len(resp.Snapshot.Sessions)))
	} else {
		check("daemon", false, "not reachable on "+l.Socket+": systemctl --user status cccp")
	}
	if out, err := (execRunner{}).Run("systemctl", "--user", "is-active", "cccp.service"); err == nil {
		check("systemd unit", true, out)
	} else {
		check("systemd unit", false, out)
	}

	plugins, _ := (execRunner{}).Run("claude", "plugin", "list")
	check("claude plugin", strings.Contains(plugins, "cccp@cccp"), "cccp@cccp in `claude plugin list`")
	if _, err := os.Stat(filepath.Join(l.AgyPluginDir, "hooks.json")); err == nil {
		check("agy plugin", true, l.AgyPluginDir)
	} else {
		check("agy plugin", false, l.AgyPluginDir+" missing")
	}

	conf, _ := os.ReadFile(l.TmuxConf)
	_, needsLine := install.AddSourceLine(conf, l.TmuxSnippet)
	check("tmux.conf", !needsLine, "sources "+l.TmuxSnippet)
	if hooks, err := a.tmux.Run("show-hooks", "-g"); err == nil {
		whooks, _ := a.tmux.Run("show-hooks", "-gw")
		n := strings.Count(hooks, "[97]") + strings.Count(whooks, "[97]")
		check("tmux hooks", n == len(install.Hooks), fmt.Sprintf("%d of %d active in the running server", n, len(install.Hooks)))
		fe, _ := a.tmux.Run("show-options", "-gv", "focus-events")
		check("tmux focus-events", strings.TrimSpace(fe) == "on", "focus-events "+strings.TrimSpace(fe))
	} else {
		check("tmux", false, err.Error())
	}

	recent := 0
	if f, err := os.Open(l.HookErrorLog); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if t, err := time.Parse(time.RFC3339, strings.SplitN(sc.Text(), " ", 2)[0]); err == nil && time.Since(t) < 24*time.Hour {
				recent++
			}
		}
		f.Close()
	}
	check("hook errors (24h)", recent == 0, fmt.Sprintf("%d (see %s)", recent, l.HookErrorLog))

	snap := a.snapshot(false)
	by := map[core.Tracking]int{}
	for _, s := range snap.Sessions {
		if s.Alive {
			by[s.Tracking]++
		}
	}
	check("live sessions", true, fmt.Sprintf("hooks+registry %d · hooks %d · registry %d · unmanaged %d",
		by[core.TrackHooksRegistry], by[core.TrackHooks], by[core.TrackRegistry], by[core.TrackUnmanaged]))
	if by[core.TrackRegistry]+by[core.TrackUnmanaged] > 0 {
		fmt.Println("  sessions without hooks started before the plugin was installed;")
		fmt.Println("  restart them (or run /reload-plugins in them) for full tracking.")
	}
	fmt.Printf("\n%d ok, %d problems\n", ok, bad)
	return nil
}
