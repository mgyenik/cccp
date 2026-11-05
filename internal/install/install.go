// Package install installs and removes cccp's integrations: the binary, the
// Claude Code plugin, the agy plugin, the systemd user unit and the tmux
// snippet. Every external effect goes through injected dependencies (a
// command runner, a tmux runner, the embedded assets, the layout), and the
// tmux.conf edits are pure functions.
package install

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cccp/internal/model"
	"cccp/internal/paths"
	"cccp/internal/store"
	"cccp/internal/tmux"
)

// Runner runs an external command and returns its combined output. env nil
// inherits cccp's environment; otherwise env is the command's complete
// environment (so a variable can be removed, not only set).
type Runner interface {
	Run(env []string, name string, args ...string) (string, error)
}

// Tmux runs tmux commands (tmux.Client).
type Tmux interface {
	Run(args ...string) (string, error)
}

// Installer holds the installation's dependencies.
type Installer struct {
	L paths.Layout
	// Profiles are the Claude profiles; the plugin is installed into each.
	Profiles []model.Profile
	Assets   fs.FS // the embedded integrations (cccp.Assets)
	Cmd      Runner
	// Environ is the environment claude commands start from (os.Environ()),
	// with each profile then selected by model.Profile.Environ.
	Environ []string

	Tmux Tmux
	Self string // path of the running binary
	Out  io.Writer
	In   *bufio.Reader
	Yes  bool // do every step without asking
}

// Step names, in order.
const (
	StepBin     = "bin"
	StepClaude  = "claude"
	StepAgy     = "agy"
	StepSystemd = "systemd"
	StepTmux    = "tmux"
)

type step struct {
	name  string
	what  string
	apply func() error
}

func (i *Installer) printf(format string, a ...any) { fmt.Fprintf(i.Out, format, a...) }

func (i *Installer) confirm() bool {
	if i.Yes {
		return true
	}
	i.printf("  proceed? [y/N] ")
	line, _ := i.In.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// Install runs the install steps (all of them when only is empty).
func (i *Installer) Install(only map[string]bool) error {
	l := i.L
	steps := []step{{StepBin, "copy this binary to " + l.BinPath, i.installBin}}
	for _, p := range i.Profiles {
		p := p
		steps = append(steps, step{StepClaude, "install the Claude Code plugin (observe-only hooks) into the " + p.Name + " profile (" + p.Dir + ")" +
			"\n    from the local marketplace at " + l.MarketplaceDir +
			"\n    takes effect in sessions started (or /reload-plugins'd) afterwards", func() error { return i.installClaude(p) }})
	}
	steps = append(steps,
		step{StepAgy, "install the agy plugin (observe-only hooks: SessionStart, PreInvocation, PostToolUse, Stop) to " + l.AgyPluginDir, i.installAgy},
		step{StepSystemd, "install and start the systemd user unit " + l.SystemdUnit, i.installSystemd},
		step{StepTmux, "add `" + SourceLine(l.TmuxSnippet) + "` to " + l.TmuxConf +
			"\n    sets focus-events on, hooks [97], Shift+PgDn / Shift+PgUp / Shift+Up, status-right and window-status formats" +
			"\n    (current values are saved to " + l.TmuxRestore + " for uninstall)", i.installTmux},
	)
	for _, s := range steps {
		if len(only) > 0 && !only[s.name] {
			continue
		}
		i.printf("\n[%s] %s\n", s.name, s.what)
		if !i.confirm() {
			i.printf("  skipped\n")
			continue
		}
		if err := s.apply(); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		i.printf("  done\n")
	}
	i.printf("\nRun `cccp doctor` to check the installation.\n")
	return nil
}

// Uninstall reverses every step, continuing past failures.
func (i *Installer) Uninstall() error {
	l := i.L
	steps := []step{
		{StepTmux, "remove the cccp line from " + l.TmuxConf + ", unset cccp's hooks and options and restore saved settings", i.uninstallTmux},
		{StepSystemd, "stop, disable and remove the systemd user unit", i.uninstallSystemd},
	}
	for _, p := range i.Profiles {
		p := p
		steps = append(steps, step{StepClaude, "uninstall the Claude Code plugin from the " + p.Name + " profile (" + p.Dir + ")", func() error { return i.uninstallClaude(p) }})
	}
	steps = append(steps, step{StepAgy, "remove " + l.AgyPluginDir, func() error { return os.RemoveAll(l.AgyPluginDir) }})
	steps = append(steps, step{"marketplace", "remove the local marketplace " + l.MarketplaceDir, func() error { return os.RemoveAll(l.MarketplaceDir) }})
	var failed []string
	for _, s := range steps {
		i.printf("\n[%s] %s\n", s.name, s.what)
		if !i.confirm() {
			i.printf("  skipped\n")
			continue
		}
		if err := s.apply(); err != nil {
			i.printf("  error: %v\n", err)
			failed = append(failed, s.name)
			continue
		}
		i.printf("  done\n")
	}
	i.printf("\nState (%s), cache (%s) and the binary (%s) are left in place; delete them by hand if you want.\n",
		l.StateDir, l.CacheDir, l.BinPath)
	if len(failed) > 0 {
		return fmt.Errorf("steps failed: %s", strings.Join(failed, ", "))
	}
	return nil
}

// ---- files ----------------------------------------------------------------------

func (i *Installer) writeAsset(src, dst string, mode os.FileMode) error {
	b, err := fs.ReadFile(i.Assets, src)
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

func (i *Installer) writeTree(src, dst string) error {
	return fs.WalkDir(i.Assets, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return i.writeAsset(p, target, 0o644)
	})
}

// ---- steps ------------------------------------------------------------------------

func (i *Installer) installBin() error {
	self, err := filepath.EvalSymlinks(i.Self)
	if err != nil {
		return err
	}
	if dst, _ := filepath.EvalSymlinks(i.L.BinPath); dst == self {
		i.printf("  already running from %s\n", i.L.BinPath)
		return nil
	}
	b, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(i.L.BinPath), 0o755); err != nil {
		return err
	}
	// Atomic replace: a running daemon keeps its (unlinked) old binary.
	if err := store.WriteAtomic(i.L.BinPath, b); err != nil {
		return err
	}
	return os.Chmod(i.L.BinPath, 0o755)
}

// claudeEnv is the complete environment for a claude command in profile p.
func (i *Installer) claudeEnv(p model.Profile) []string { return p.Environ(i.Environ) }

func (i *Installer) installClaude(p model.Profile) error {
	mp := i.L.MarketplaceDir
	env := i.claudeEnv(p)
	if err := i.writeAsset(".claude-plugin/marketplace.json", filepath.Join(mp, ".claude-plugin", "marketplace.json"), 0o644); err != nil {
		return err
	}
	if err := i.writeTree("integrations/claude-plugin", filepath.Join(mp, "integrations", "claude-plugin")); err != nil {
		return err
	}
	list, err := i.Cmd.Run(env, "claude", "plugin", "marketplace", "list")
	if err != nil {
		return fmt.Errorf("claude plugin marketplace list: %v: %s", err, list)
	}
	if !strings.Contains(list, mp) {
		if out, err := i.Cmd.Run(env, "claude", "plugin", "marketplace", "add", mp); err != nil {
			return fmt.Errorf("marketplace add: %v: %s", err, out)
		}
	}
	plugins, err := i.Cmd.Run(env, "claude", "plugin", "list")
	if err != nil {
		return fmt.Errorf("claude plugin list: %v: %s", err, plugins)
	}
	if strings.Contains(plugins, "cccp@cccp") {
		i.printf("  plugin already installed; files updated in place\n")
		return nil
	}
	if out, err := i.Cmd.Run(env, "claude", "plugin", "install", "cccp@cccp"); err != nil {
		return fmt.Errorf("plugin install: %v: %s", err, out)
	}
	return nil
}

func (i *Installer) installAgy() error {
	cfg := filepath.Dir(filepath.Dir(i.L.AgyPluginDir)) // ~/.gemini/config
	if _, err := os.Stat(cfg); err != nil {
		i.printf("  %s not found; skipping (agy not installed?)\n", cfg)
		return nil
	}
	return i.writeTree("integrations/agy-plugin", i.L.AgyPluginDir)
}

func (i *Installer) installSystemd() error {
	if err := i.writeAsset("integrations/systemd/cccp.service", i.L.SystemdUnit, 0o644); err != nil {
		return err
	}
	// restart, not start: picks up a replaced binary on reinstall.
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "cccp.service"}, {"restart", "cccp.service"}} {
		if out, err := i.Cmd.Run(nil, "systemctl", append([]string{"--user"}, args...)...); err != nil {
			return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

// RestoredOptions are the tmux options cccp.tmux changes; their values from
// before the install are saved so uninstall can put them back.
var RestoredOptions = []string{"focus-events", "status-right", "status-right-length", "window-status-format", "window-status-current-format",
	"detach-on-destroy", "exit-empty"}

// Key is a tmux key binding: a key table and a key.
type Key struct{ Table, Key string }

func (k Key) String() string { return k.Table + " " + k.Key }

// RestoredKeys are the keys cccp.tmux binds.
var RestoredKeys = []Key{{"root", "S-NPage"}, {"root", "S-PPage"}, {"root", "S-Up"}}

// parseSavedKey reads a Saved.Keys entry: "table key", or a bare key from
// installs before the keys moved to the root table (the prefix table).
func parseSavedKey(s string) Key {
	if t, k, ok := strings.Cut(s, " "); ok {
		return Key{Table: t, Key: k}
	}
	return Key{Table: "prefix", Key: s}
}

// Hooks are the tmux hooks cccp.tmux sets, at index [97].
var Hooks = []string{
	"client-session-changed", "session-window-changed", "window-pane-changed", "client-focus-in",
	"client-focus-out", "client-attached", "client-detached", "after-new-window", "after-split-window",
	"pane-exited", "session-created", "session-closed", "session-renamed", "window-renamed",
	"window-layout-changed", "window-linked", "window-unlinked",
}

// Saved is the tmux state cccp.tmux overrides. It is stored as data and
// restored with set-option calls, so no value has to survive config-file
// quoting. Keys maps "table key" to tmux's own list-keys output for that
// key before cccp bound it ("" = it was unbound).
type Saved struct {
	Options map[string]string `json:"options"`
	Keys    map[string]string `json:"keys"`
}

func (i *Installer) installTmux() error {
	l := i.L
	if err := i.writeAsset("integrations/tmux/cccp.tmux", l.TmuxSnippet, 0o644); err != nil {
		return err
	}
	if err := i.saveTmuxState(); err != nil {
		return err
	}
	conf, err := os.ReadFile(l.TmuxConf)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if next, changed := AddSourceLine(conf, l.TmuxSnippet); changed {
		if len(conf) > 0 {
			if err := store.WriteAtomic(l.TmuxConf+".cccp-backup", conf); err != nil {
				return err
			}
		}
		if err := store.WriteAtomic(l.TmuxConf, next); err != nil {
			return err
		}
	}
	if _, err := i.Tmux.Run("source-file", l.TmuxSnippet); err != nil {
		if errors.Is(err, tmux.ErrNoServer) {
			return nil
		}
		return err
	}
	if stale := i.staleFocusClients(); len(stale) > 0 {
		fmt.Fprintf(i.Out, "    %s\n", ReattachAdvice(stale))
	}
	return nil
}

// staleFocusClients are the attached clients whose terminals were never
// asked for focus reports (model.TmuxClient.FocusReported).
func (i *Installer) staleFocusClients() []tmux.ClientInfo {
	out, err := i.Tmux.Run("list-clients", "-F", tmux.ClientFormat)
	if err != nil {
		return nil
	}
	return StaleFocusClients(tmux.ParseClients(out))
}

// StaleFocusClients filters clients whose focus is not reported.
func StaleFocusClients(cs []tmux.ClientInfo) []tmux.ClientInfo {
	var out []tmux.ClientInfo
	for _, c := range cs {
		if !c.FocusReported() {
			out = append(out, c)
		}
	}
	return out
}

// ReattachAdvice tells the user which clients to reattach, and why.
func ReattachAdvice(stale []tmux.ClientInfo) string {
	var names []string
	for _, c := range stale {
		names = append(names, c.Name+" (session "+c.Session+")")
	}
	return "these tmux clients attached before focus reporting was on, so tmux never learns when you look away from them" +
		" and cccp treats them as not looking (notifications even while you watch); detach (prefix d) and reattach each: " +
		strings.Join(names, ", ")
}

func (i *Installer) uninstallTmux() error {
	l := i.L
	if conf, err := os.ReadFile(l.TmuxConf); err == nil {
		if err := store.WriteAtomic(l.TmuxConf, RemoveSourceLine(conf, l.TmuxSnippet)); err != nil {
			return err
		}
	}
	for _, h := range Hooks {
		_, _ = i.Tmux.Run("set-hook", "-gu", h+"[97]")
	}
	_, _ = i.Tmux.Run("set-option", "-gqu", "@cccp_status")
	_, _ = i.Tmux.Run("set-option", "-gqu", tmux.FocusSinceOption)

	b, err := os.ReadFile(l.TmuxRestore)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved Saved
	if err := json.Unmarshal(b, &saved); err != nil {
		return fmt.Errorf("%s: %w", l.TmuxRestore, err)
	}
	for _, o := range RestoredOptions {
		v, ok := saved.Options[o]
		if !ok {
			continue
		}
		if _, err := i.Tmux.Run("set-option", "-g", o, v); err != nil && !errors.Is(err, tmux.ErrNoServer) {
			return err
		}
	}
	for _, name := range sortedKeys(saved.Keys) {
		if err := i.restoreKey(parseSavedKey(name), saved.Keys[name]); err != nil {
			return err
		}
	}
	return os.Remove(l.TmuxRestore)
}

// saveTmuxState records, once, the tmux state cccp.tmux is about to change.
// Options are saved on the first install only: a reinstall must not save
// cccp's own values. Keys are saved per key, so a newly bound key is saved
// on the install that first binds it, and keys an older version bound but
// this one does not are restored now.
func (i *Installer) saveTmuxState() error {
	l := i.L
	saved := Saved{Options: map[string]string{}, Keys: map[string]string{}}
	b, err := os.ReadFile(l.TmuxRestore)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &saved); err != nil {
			return fmt.Errorf("%s: %w", l.TmuxRestore, err)
		}
		if saved.Options == nil {
			saved.Options = map[string]string{}
		}
		if saved.Keys == nil {
			saved.Keys = map[string]string{}
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return err
	}
	// Options are saved one by one, like keys: an option a newer version
	// sets for the first time is saved before its first change, even when
	// the others were saved by an earlier install.
	for _, o := range RestoredOptions {
		if _, ok := saved.Options[o]; ok {
			continue
		}
		if v, err := i.Tmux.Run("show-options", "-gv", o); err == nil {
			saved.Options[o] = strings.TrimSuffix(v, "\n")
		}
	}
	want := map[string]bool{}
	for _, k := range RestoredKeys {
		want[k.String()] = true
	}
	keys := map[string]string{}
	for _, name := range sortedKeys(saved.Keys) {
		k := parseSavedKey(name)
		if want[k.String()] {
			keys[k.String()] = saved.Keys[name]
			continue
		}
		// Bound by an earlier version only: put it back as it was.
		if err := i.restoreKey(k, saved.Keys[name]); err != nil {
			return err
		}
	}
	for _, k := range RestoredKeys {
		if _, ok := keys[k.String()]; !ok {
			out, _ := i.Tmux.Run("list-keys", "-T", k.Table, k.Key)
			keys[k.String()] = strings.TrimSpace(out)
		}
	}
	saved.Keys = keys
	b, err = json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(l.TmuxRestore, b)
}

// restoreKey puts a key back as it was: unbound, or its saved binding (tmux's
// own list-keys output, which is valid config, sourced back).
func (i *Installer) restoreKey(k Key, line string) error {
	if line == "" {
		_, _ = i.Tmux.Run("unbind-key", "-T", k.Table, k.Key)
		return nil
	}
	tmp := i.L.TmuxRestore + ".keys"
	if err := os.WriteFile(tmp, []byte(line+"\n"), 0o600); err != nil {
		return err
	}
	_, err := i.Tmux.Run("source-file", tmp)
	os.Remove(tmp)
	if err != nil && !errors.Is(err, tmux.ErrNoServer) {
		return err
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (i *Installer) uninstallSystemd() error {
	_, _ = i.Cmd.Run(nil, "systemctl", "--user", "disable", "--now", "cccp.service")
	if err := os.Remove(i.L.SystemdUnit); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, _ = i.Cmd.Run(nil, "systemctl", "--user", "daemon-reload")
	return nil
}

func (i *Installer) uninstallClaude(p model.Profile) error {
	env := i.claudeEnv(p)
	plugins, err := i.Cmd.Run(env, "claude", "plugin", "list")
	if err != nil {
		return fmt.Errorf("claude plugin list: %v: %s", err, plugins)
	}
	if strings.Contains(plugins, "cccp@cccp") {
		if out, err := i.Cmd.Run(env, "claude", "plugin", "uninstall", "cccp@cccp"); err != nil {
			return fmt.Errorf("plugin uninstall: %v: %s", err, out)
		}
	}
	list, err := i.Cmd.Run(env, "claude", "plugin", "marketplace", "list")
	if err != nil {
		return fmt.Errorf("claude plugin marketplace list: %v: %s", err, list)
	}
	if strings.Contains(list, i.L.MarketplaceDir) {
		if out, err := i.Cmd.Run(env, "claude", "plugin", "marketplace", "remove", "cccp"); err != nil {
			return fmt.Errorf("marketplace remove: %v: %s", err, out)
		}
	}
	return nil
}

// ---- tmux.conf edits (pure) -----------------------------------------------------

// SourceLine is the line install adds to tmux.conf.
func SourceLine(snippet string) string { return "source-file " + snippet + "  # cccp" }

// AddSourceLine appends the source line unless the configuration already
// sources the snippet. It reports whether conf changed.
func AddSourceLine(conf []byte, snippet string) ([]byte, bool) {
	if sourcesSnippet(conf, snippet) {
		return conf, false
	}
	out := append([]byte(nil), conf...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, []byte("\n"+SourceLine(snippet)+"\n")...), true
}

// RemoveSourceLine removes what AddSourceLine added (the line and the blank
// line before it), and any other line sourcing the snippet.
func RemoveSourceLine(conf []byte, snippet string) []byte {
	s := strings.TrimSuffix(string(conf), "\n"+SourceLine(snippet)+"\n")
	var keep []string
	for _, l := range strings.SplitAfter(s, "\n") {
		if !isSourceLine(l, snippet) {
			keep = append(keep, l)
		}
	}
	return []byte(strings.Join(keep, ""))
}

func sourcesSnippet(conf []byte, snippet string) bool {
	for _, l := range strings.Split(string(conf), "\n") {
		if isSourceLine(l, snippet) {
			return true
		}
	}
	return false
}

// isSourceLine reports whether a config line sources the snippet:
// `source-file [-q] PATH [# comment]` (or `source`), not commented out.
func isSourceLine(line, snippet string) bool {
	if i := strings.Index(line, "#"); i >= 0 {
		line = line[:i]
	}
	f := strings.Fields(line)
	if len(f) < 2 || (f[0] != "source-file" && f[0] != "source") {
		return false
	}
	return strings.Trim(f[len(f)-1], `"'`) == snippet
}
