package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"cccp/internal/engine"
	"cccp/internal/ipc"
	"cccp/internal/paths"
	"cccp/internal/tmux"
)

// runDoctor checks each part of the installation and reports what it found.
func runDoctor(args []string) error {
	ok, bad := 0, 0
	check := func(name string, pass bool, detail string) {
		mark := "✓"
		if pass {
			ok++
		} else {
			mark = "✗"
			bad++
		}
		fmt.Printf("%s %-22s %s\n", mark, name, detail)
	}

	if fi, err := os.Stat(binPath()); err == nil && fi.Mode()&0o111 != 0 {
		check("binary", true, binPath())
	} else {
		check("binary", false, binPath()+" missing (hooks fall back to a silent no-op)")
	}

	resp, err := ipc.Call(ipc.Request{Op: ipc.OpSnapshot}, 2*time.Second)
	if err == nil && resp.Snapshot != nil {
		check("daemon", true, fmt.Sprintf("answering on %s (%d sessions)", paths.Socket(), len(resp.Snapshot.Sessions)))
	} else {
		check("daemon", false, "not reachable on "+paths.Socket()+": systemctl --user status cccp")
	}
	if out, err := run("systemctl", "--user", "is-active", "cccp.service"); err == nil {
		check("systemd unit", true, out)
	} else {
		check("systemd unit", false, strings.TrimSpace(out))
	}

	plugins, _ := run("claude", "plugin", "list")
	check("claude plugin", strings.Contains(plugins, "cccp@cccp"), "cccp@cccp in `claude plugin list`")
	if _, err := os.Stat(paths.AgyPluginDir() + "/hooks.json"); err == nil {
		check("agy plugin", true, paths.AgyPluginDir())
	} else {
		check("agy plugin", false, paths.AgyPluginDir()+" missing")
	}

	tm := tmux.Default()
	conf, _ := os.ReadFile(tmuxConf())
	check("tmux.conf", strings.Contains(string(conf), tmuxSnippet()), "sources "+tmuxSnippet())
	if hooks, err := tm.Run("show-hooks", "-g"); err == nil {
		n := strings.Count(hooks, "[97]")
		whooks, _ := tm.Run("show-hooks", "-gw")
		n += strings.Count(whooks, "[97]")
		check("tmux hooks", n == len(tmuxHooks), fmt.Sprintf("%d of %d active in the running server", n, len(tmuxHooks)))
		fe, _ := tm.Run("show-options", "-gv", "focus-events")
		check("tmux focus-events", strings.TrimSpace(fe) == "on", "focus-events "+strings.TrimSpace(fe))
	} else {
		check("tmux", false, err.Error())
	}

	// Recent hook errors.
	if f, err := os.Open(paths.StateDir() + "/hook-errors.log"); err == nil {
		var lines []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		f.Close()
		recent := 0
		for _, l := range lines {
			if t, err := time.Parse(time.RFC3339, strings.SplitN(l, " ", 2)[0]); err == nil && time.Since(t) < 24*time.Hour {
				recent++
			}
		}
		check("hook errors (24h)", recent == 0, fmt.Sprintf("%d (see %s/hook-errors.log)", recent, paths.StateDir()))
	} else {
		check("hook errors (24h)", true, "none logged")
	}

	// How sessions are tracked.
	snap, err := snapshot(false)
	if err == nil {
		by := map[engine.Tracking]int{}
		for _, s := range snap.Sessions {
			if s.Alive {
				by[s.Tracking]++
			}
		}
		detail := fmt.Sprintf("hooks+registry %d · hooks %d · registry %d · unmanaged %d",
			by[engine.TrackHooksRegistry], by[engine.TrackHooks], by[engine.TrackRegistry], by[engine.TrackUnmanaged])
		check("live sessions", true, detail)
		if by[engine.TrackRegistry]+by[engine.TrackUnmanaged] > 0 {
			fmt.Println("  sessions without hooks started before the plugin was installed;")
			fmt.Println("  restart them (or run /reload-plugins in them) for full tracking.")
		}
	}
	fmt.Printf("\n%d ok, %d problems\n", ok, bad)
	return nil
}
