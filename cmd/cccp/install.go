package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"cccp"
	"cccp/internal/paths"
	"cccp/internal/store"
	"cccp/internal/tmux"
)

// Locations written by `cccp install`.
func shareDir() string       { return filepath.Join(paths.Home(), ".local", "share", "cccp") }
func marketplaceDir() string { return filepath.Join(shareDir(), "marketplace") }
func tmuxSnippet() string    { return filepath.Join(shareDir(), "cccp.tmux") }
func tmuxRestore() string    { return filepath.Join(shareDir(), "tmux-restore.json") }
func binPath() string        { return filepath.Join(paths.Home(), ".local", "bin", "cccp") }
func unitPath() string {
	return filepath.Join(paths.Home(), ".config", "systemd", "user", "cccp.service")
}
func tmuxConf() string { return filepath.Join(paths.Home(), ".tmux.conf") }

func sourceLine() string { return "source-file " + tmuxSnippet() + "  # cccp" }

type step struct {
	name  string
	what  string // shown before asking
	apply func() error
}

func confirm(in *bufio.Reader, yes bool, q string) bool {
	if yes {
		return true
	}
	fmt.Printf("%s [y/N] ", q)
	line, _ := in.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func runInstall(args []string) error {
	fset := flag.NewFlagSet("install", flag.ExitOnError)
	yes := fset.Bool("yes", false, "do every step without asking")
	only := fset.String("only", "", "comma-separated subset: bin,claude,agy,systemd,tmux")
	_ = fset.Parse(args)
	want := map[string]bool{}
	for _, s := range strings.Split(*only, ",") {
		if s = strings.TrimSpace(s); s != "" {
			want[s] = true
		}
	}
	steps := []step{
		{"bin", "copy this binary to " + binPath(), installBin},
		{"claude", "install the Claude Code plugin (observe-only hooks) from a local marketplace at " + marketplaceDir() +
			"\n    runs: claude plugin marketplace add …; claude plugin install cccp@cccp" +
			"\n    takes effect in sessions started (or /reload-plugins'd) afterwards", installClaude},
		{"agy", "install the agy plugin (observe-only hooks: SessionStart, PreInvocation, PostToolUse, Stop) to " + paths.AgyPluginDir(), installAgy},
		{"systemd", "install and start the systemd user unit " + unitPath(), installSystemd},
		{"tmux", "add `" + sourceLine() + "` to " + tmuxConf() +
			"\n    sets focus-events on, hooks [97], prefix a / prefix A, status-right and window-status formats" +
			"\n    (current values are saved to " + tmuxRestore() + " for uninstall)", installTmux},
	}
	in := bufio.NewReader(os.Stdin)
	for _, s := range steps {
		if len(want) > 0 && !want[s.name] {
			continue
		}
		fmt.Printf("\n[%s] %s\n", s.name, s.what)
		if !confirm(in, *yes, "  proceed?") {
			fmt.Println("  skipped")
			continue
		}
		if err := s.apply(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		fmt.Println("  done")
	}
	fmt.Println("\nRun `cccp doctor` to check the installation.")
	return nil
}

// writeAsset copies one embedded file to dst (atomically).
func writeAsset(src, dst string, mode os.FileMode) error {
	b, err := cccp.Assets.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := store.WriteAtomic(dst, b); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// writeTree copies an embedded directory to dst.
func writeTree(src, dst string) error {
	return fs.WalkDir(cccp.Assets, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return writeAsset(p, target, 0o644)
	})
}

func installBin() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)
	if dst, _ := filepath.EvalSymlinks(binPath()); dst == self {
		fmt.Println("  already running from", binPath())
		return nil
	}
	f, err := os.Open(self)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(binPath()), 0o755); err != nil {
		return err
	}
	// Atomic replace: a running daemon keeps its (unlinked) old binary.
	if err := store.WriteAtomic(binPath(), b); err != nil {
		return err
	}
	return os.Chmod(binPath(), 0o755)
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func installClaude() error {
	if err := writeAsset(".claude-plugin/marketplace.json", filepath.Join(marketplaceDir(), ".claude-plugin", "marketplace.json"), 0o644); err != nil {
		return err
	}
	if err := writeTree("integrations/claude-plugin", filepath.Join(marketplaceDir(), "integrations", "claude-plugin")); err != nil {
		return err
	}
	list, _ := run("claude", "plugin", "marketplace", "list")
	if !strings.Contains(list, marketplaceDir()) {
		if out, err := run("claude", "plugin", "marketplace", "add", marketplaceDir()); err != nil {
			return fmt.Errorf("marketplace add: %v: %s", err, out)
		}
	}
	plugins, _ := run("claude", "plugin", "list")
	if strings.Contains(plugins, "cccp@cccp") {
		fmt.Println("  plugin already installed; files updated in place")
		return nil
	}
	if out, err := run("claude", "plugin", "install", "cccp@cccp"); err != nil {
		return fmt.Errorf("plugin install: %v: %s", err, out)
	}
	return nil
}

func installAgy() error {
	if _, err := os.Stat(filepath.Dir(filepath.Dir(paths.AgyPluginDir()))); err != nil {
		fmt.Println("  ~/.gemini/config not found; skipping (agy not installed?)")
		return nil
	}
	return writeTree("integrations/agy-plugin", paths.AgyPluginDir())
}

func installSystemd() error {
	if err := writeAsset("integrations/systemd/cccp.service", unitPath(), 0o644); err != nil {
		return err
	}
	if out, err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %v: %s", err, out)
	}
	if out, err := run("systemctl", "--user", "enable", "cccp.service"); err != nil {
		return fmt.Errorf("enable: %v: %s", err, out)
	}
	// restart, not start: picks up a replaced binary on reinstall.
	if out, err := run("systemctl", "--user", "restart", "cccp.service"); err != nil {
		return fmt.Errorf("restart: %v: %s", err, out)
	}
	return nil
}

// tmuxRestoredOptions are the options cccp.tmux changes; their current values
// are saved so uninstall can put them back.
var tmuxRestoredOptions = []string{"focus-events", "status-right", "status-right-length", "window-status-format", "window-status-current-format"}

func installTmux() error {
	if err := writeAsset("integrations/tmux/cccp.tmux", tmuxSnippet(), 0o644); err != nil {
		return err
	}
	tm := tmux.Default()
	if _, err := os.Stat(tmuxRestore()); errors.Is(err, os.ErrNotExist) {
		// Saved as data and restored with set-option calls, so no value
		// has to survive tmux config quoting.
		r := tmuxSaved{Options: map[string]string{}, Keys: map[string]string{}}
		for _, o := range tmuxRestoredOptions {
			if v, err := tm.Run("show-options", "-gv", o); err == nil {
				r.Options[o] = strings.TrimSuffix(v, "\n")
			}
		}
		for _, k := range []string{"a", "A"} {
			out, _ := tm.Run("list-keys", "-T", "prefix", k)
			r.Keys[k] = strings.TrimSpace(out) // tmux's own, valid config syntax; "" = unbound
		}
		b, _ := json.MarshalIndent(r, "", "  ")
		if err := store.WriteAtomic(tmuxRestore(), b); err != nil {
			return err
		}
	}
	conf, err := os.ReadFile(tmuxConf())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !bytes.Contains(conf, []byte(tmuxSnippet())) {
		if len(conf) > 0 {
			if err := store.WriteAtomic(tmuxConf()+".cccp-backup", conf); err != nil {
				return err
			}
		}
		add := conf
		if len(add) > 0 && add[len(add)-1] != '\n' {
			add = append(add, '\n')
		}
		add = append(add, []byte("\n"+sourceLine()+"\n")...)
		if err := store.WriteAtomic(tmuxConf(), add); err != nil {
			return err
		}
	}
	if _, err := tm.Run("source-file", tmuxSnippet()); err != nil && !errors.Is(err, tmux.ErrNoServer) {
		return err
	}
	return nil
}

// tmuxSaved is the tmux state cccp.tmux overrides, saved for uninstall.
type tmuxSaved struct {
	Options map[string]string `json:"options"`
	Keys    map[string]string `json:"keys"`
}

var tmuxHooks = []string{
	"client-session-changed", "session-window-changed", "window-pane-changed", "client-focus-in",
	"client-focus-out", "client-attached", "client-detached", "after-new-window", "after-split-window",
	"pane-exited", "session-created", "session-closed",
}

func runUninstall(args []string) error {
	fset := flag.NewFlagSet("uninstall", flag.ExitOnError)
	yes := fset.Bool("yes", false, "do every step without asking")
	_ = fset.Parse(args)
	in := bufio.NewReader(os.Stdin)
	steps := []step{
		{"tmux", "remove the cccp line from " + tmuxConf() + ", unset cccp's hooks/options and restore saved settings", uninstallTmux},
		{"systemd", "stop, disable and remove the systemd user unit", uninstallSystemd},
		{"claude", "uninstall the Claude Code plugin and remove the cccp marketplace", uninstallClaude},
		{"agy", "remove " + paths.AgyPluginDir(), func() error { return os.RemoveAll(paths.AgyPluginDir()) }},
	}
	for _, s := range steps {
		fmt.Printf("\n[%s] %s\n", s.name, s.what)
		if !confirm(in, *yes, "  proceed?") {
			fmt.Println("  skipped")
			continue
		}
		if err := s.apply(); err != nil {
			fmt.Printf("  error: %v\n", err)
			continue
		}
		fmt.Println("  done")
	}
	fmt.Printf("\nState (%s), cache (%s) and the binary (%s) are left in place; delete them by hand if you want.\n",
		paths.StateDir(), paths.CacheDir(), binPath())
	return nil
}

func uninstallTmux() error {
	if conf, err := os.ReadFile(tmuxConf()); err == nil {
		// Remove exactly what install added ("\n" + line + "\n"); fall
		// back to dropping any line that sources the snippet.
		conf = bytes.Replace(conf, []byte("\n"+sourceLine()+"\n"), nil, 1)
		var keep []string
		for _, l := range strings.Split(string(conf), "\n") {
			if strings.Contains(l, tmuxSnippet()) {
				continue
			}
			keep = append(keep, l)
		}
		if err := store.WriteAtomic(tmuxConf(), []byte(strings.Join(keep, "\n"))); err != nil {
			return err
		}
	}
	tm := tmux.Default()
	for _, h := range tmuxHooks {
		_, _ = tm.Run("set-hook", "-gu", h+"[97]")
	}
	_, _ = tm.Run("set-option", "-gqu", "@cccp_status")
	b, err := os.ReadFile(tmuxRestore())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var r tmuxSaved
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("%s: %w", tmuxRestore(), err)
	}
	for o, v := range r.Options {
		if _, err := tm.Run("set-option", "-g", o, v); err != nil && !errors.Is(err, tmux.ErrNoServer) {
			return err
		}
	}
	for k, line := range r.Keys {
		if line == "" {
			_, _ = tm.Run("unbind-key", "-T", "prefix", k)
			continue
		}
		tmp := tmuxRestore() + ".keys"
		if err := os.WriteFile(tmp, []byte(line+"\n"), 0o600); err != nil {
			return err
		}
		_, err := tm.Run("source-file", tmp)
		os.Remove(tmp)
		if err != nil && !errors.Is(err, tmux.ErrNoServer) {
			return err
		}
	}
	return os.Remove(tmuxRestore())
}

func uninstallSystemd() error {
	_, _ = run("systemctl", "--user", "disable", "--now", "cccp.service")
	if err := os.Remove(unitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, _ = run("systemctl", "--user", "daemon-reload")
	return nil
}

func uninstallClaude() error {
	if out, err := run("claude", "plugin", "uninstall", "cccp@cccp"); err != nil && !strings.Contains(out, "not") {
		return fmt.Errorf("%v: %s", err, out)
	}
	if out, err := run("claude", "plugin", "marketplace", "remove", "cccp"); err != nil && !strings.Contains(out, "not") {
		return fmt.Errorf("%v: %s", err, out)
	}
	return os.RemoveAll(marketplaceDir())
}
