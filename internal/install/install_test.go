package install

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cccp"
	"cccp/internal/model"
	"cccp/internal/paths"
)

// --- tmux.conf edits ------------------------------------------------------------

const snip = "/h/.local/share/cccp/cccp.tmux"

func TestAddRemoveSourceLineRoundTrip(t *testing.T) {
	for _, orig := range []string{"", "set -g mouse on\n", "bind a x\nset -g mouse on\n"} {
		added, changed := AddSourceLine([]byte(orig), snip)
		if !changed || !strings.Contains(string(added), SourceLine(snip)) {
			t.Fatalf("%q: not added: %q", orig, added)
		}
		if again, changed := AddSourceLine(added, snip); changed || string(again) != string(added) {
			t.Fatalf("%q: second add changed the file: %q", orig, again)
		}
		if back := string(RemoveSourceLine(added, snip)); back != orig {
			t.Fatalf("round trip: got %q want %q", back, orig)
		}
	}
}

func TestSourceLineMatching(t *testing.T) {
	for line, want := range map[string]bool{
		"source-file " + snip:           true,
		"source-file -q '" + snip + "'": true,
		"source " + snip + "   # mine":  true,
		"# source-file " + snip:         false,
		"source-file /other/file.tmux":  false,
		"set -g status-right " + snip:   false,
	} {
		if got := isSourceLine(line, snip); got != want {
			t.Errorf("%q: got %v", line, got)
		}
	}
	// A hand-written line counts as already installed, and is removed.
	conf := []byte("set -g mouse on\nsource-file -q " + snip + "\n")
	if _, changed := AddSourceLine(conf, snip); changed {
		t.Fatal("added a second source line")
	}
	if got := string(RemoveSourceLine(conf, snip)); got != "set -g mouse on\n" {
		t.Fatalf("%q", got)
	}
}

// --- install/uninstall with fake runners ------------------------------------------

type fakeCmd struct {
	calls   []string
	outputs map[string]string // "name args…" → output
}

// Run records "[PROFILE ]command", where PROFILE is how a complete
// environment selects the Claude profile: "CLAUDE_CONFIG_DIR=dir", or
// "env -u CLAUDE_CONFIG_DIR" when the variable is removed (the default
// profile). A complete environment that lost the rest of cccp's environment
// is recorded as such, so tests catch it.
func (f *fakeCmd) Run(env []string, name string, args ...string) (string, error) {
	c := strings.Join(append([]string{name}, args...), " ")
	key := c
	if env != nil {
		sel, inherited := "env -u CLAUDE_CONFIG_DIR", false
		for _, kv := range env {
			if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
				sel = kv
			}
			inherited = inherited || kv == "PATH=/test/bin"
		}
		if !inherited {
			sel += " (environment lost)"
		}
		key = sel + " " + c
	}
	f.calls = append(f.calls, key)
	if out, ok := f.outputs[key]; ok {
		return out, nil
	}
	return f.outputs[c], nil
}

type fakeTmux struct {
	options map[string]string
	keys    map[string]string
	calls   []string
	clients string // list-clients output
}

func (f *fakeTmux) Run(args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case len(args) == 3 && args[0] == "show-options":
		return f.options[args[2]] + "\n", nil
	case len(args) == 4 && args[0] == "list-keys": // list-keys -T TABLE KEY
		return f.keys[args[2]+" "+args[3]], nil
	case len(args) == 4 && args[0] == "set-option" && args[1] == "-g":
		f.options[args[2]] = args[3]
	case len(args) == 3 && args[0] == "list-clients":
		return f.clients, nil
	case len(args) == 4 && args[0] == "unbind-key": // unbind-key -T TABLE KEY
		delete(f.keys, args[2]+" "+args[3])
	case len(args) == 2 && args[0] == "source-file" && strings.HasSuffix(args[1], ".keys"):
		// "bind-key -T TABLE KEY command…"
		b, _ := os.ReadFile(args[1])
		line := strings.TrimSpace(string(b))
		if w := strings.Fields(line); len(w) >= 4 {
			f.keys[w[2]+" "+w[3]] = line
		}
	}
	return "", nil
}

func setup(t *testing.T) (*Installer, *fakeCmd, *fakeTmux, paths.Layout) {
	t.Helper()
	home := t.TempDir()
	l := paths.Resolve(func(string) string { return "" }, home, 1000)
	os.MkdirAll(filepath.Join(home, ".gemini", "config"), 0o755)
	os.WriteFile(l.TmuxConf, []byte("set -g mouse on\n"), 0o644)
	self := filepath.Join(home, "build", "cccp")
	os.MkdirAll(filepath.Dir(self), 0o755)
	os.WriteFile(self, []byte("#!binary"), 0o755)
	cmd := &fakeCmd{outputs: map[string]string{}}
	tm := &fakeTmux{
		options: map[string]string{"status-right": "it's #H", "focus-events": "off", "status-right-length": "40",
			"window-status-format": "#I:#W", "window-status-current-format": "#I:#W"},
		// S-PgUp was bound before cccp; S-PgDn was not.
		keys: map[string]string{"root S-PPage": "bind-key -T root S-PPage copy-mode -u"},
	}
	profiles := []model.Profile{
		{Name: "personal", Dir: l.DefaultClaudeDir, Default: true},
		{Name: "work", Dir: filepath.Join(home, ".claude-work")},
	}
	// The inherited environment names the work profile, as in a shell
	// started through the claude-work alias: each profile must override it.
	environ := []string{"PATH=/test/bin", "CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude-work")}
	return &Installer{L: l, Profiles: profiles, Assets: cccp.Assets, Cmd: cmd, Environ: environ, Tmux: tm, Self: self, Out: io.Discard,
		In: bufio.NewReader(strings.NewReader("")), Yes: true}, cmd, tm, l
}

func TestInstallEverything(t *testing.T) {
	in, cmd, tm, l := setup(t)
	if err := in.Install(nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		l.BinPath,
		filepath.Join(l.MarketplaceDir, ".claude-plugin", "marketplace.json"),
		filepath.Join(l.MarketplaceDir, "integrations", "claude-plugin", "hooks", "hooks.json"),
		filepath.Join(l.AgyPluginDir, "hooks.json"),
		l.SystemdUnit, l.TmuxSnippet, l.TmuxRestore,
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	var wantCmds []string
	// The default profile with CLAUDE_CONFIG_DIR removed (set to ~/.claude,
	// Claude Code would use a blank ~/.claude/.claude.json), the work profile
	// with it set.
	for _, sel := range []string{"env -u CLAUDE_CONFIG_DIR", "CLAUDE_CONFIG_DIR=" + filepath.Join(l.Home, ".claude-work")} {
		env := sel + " "
		wantCmds = append(wantCmds,
			env+"claude plugin marketplace list",
			env+"claude plugin marketplace add "+l.MarketplaceDir,
			env+"claude plugin list",
			env+"claude plugin install cccp@cccp")
	}
	wantCmds = append(wantCmds,
		"systemctl --user daemon-reload",
		"systemctl --user enable cccp.service",
		"systemctl --user restart cccp.service",
	)
	if !reflect.DeepEqual(cmd.calls, wantCmds) {
		t.Fatalf("commands:\n%s", strings.Join(cmd.calls, "\n"))
	}
	conf, _ := os.ReadFile(l.TmuxConf)
	if !strings.Contains(string(conf), SourceLine(l.TmuxSnippet)) {
		t.Fatalf("tmux.conf %q", conf)
	}
	sourced := false
	for _, c := range tm.calls {
		sourced = sourced || c == "source-file "+l.TmuxSnippet
	}
	if !sourced {
		t.Fatalf("snippet not sourced: %v", tm.calls)
	}
	var saved Saved
	b, _ := os.ReadFile(l.TmuxRestore)
	json.Unmarshal(b, &saved)
	want := map[string]string{"root S-NPage": "", "root S-PPage": "bind-key -T root S-PPage copy-mode -u", "root S-Up": ""}
	if saved.Options["status-right"] != "it's #H" || !reflect.DeepEqual(saved.Keys, want) {
		t.Fatalf("saved %+v", saved)
	}
}

func TestReinstallKeepsOriginalTmuxStateAndSkipsInstalledPlugin(t *testing.T) {
	in, cmd, tm, l := setup(t)
	in.Install(nil)
	// cccp's snippet has changed the live options since.
	tm.options["status-right"] = "#{@cccp_status} …"
	cmd.calls = nil
	cmd.outputs["claude plugin marketplace list"] = "cccp  " + l.MarketplaceDir
	cmd.outputs["claude plugin list"] = "cccp@cccp  enabled"
	if err := in.Install(nil); err != nil {
		t.Fatal(err)
	}
	var saved Saved
	b, _ := os.ReadFile(l.TmuxRestore)
	json.Unmarshal(b, &saved)
	if saved.Options["status-right"] != "it's #H" {
		t.Fatalf("reinstall overwrote the saved original: %q", saved.Options["status-right"])
	}
	for _, c := range cmd.calls {
		if strings.Contains(c, "install cccp@cccp") || strings.Contains(c, "marketplace add") {
			t.Fatalf("re-ran %q", c)
		}
	}
	conf, _ := os.ReadFile(l.TmuxConf)
	if strings.Count(string(conf), SourceLine(l.TmuxSnippet)) != 1 {
		t.Fatalf("tmux.conf %q", conf)
	}
}

func TestInstallOnlyAndDeclined(t *testing.T) {
	in, cmd, _, l := setup(t)
	if err := in.Install(map[string]bool{StepBin: true}); err != nil {
		t.Fatal(err)
	}
	if len(cmd.calls) != 0 {
		t.Fatalf("--only bin ran %v", cmd.calls)
	}
	if b, _ := os.ReadFile(l.BinPath); string(b) != "#!binary" {
		t.Fatal("binary not copied")
	}
	in2, cmd2, _, l2 := setup(t)
	in2.Yes = false
	in2.In = bufio.NewReader(strings.NewReader("n\nn\nn\nn\nn\n"))
	in2.Install(nil)
	if len(cmd2.calls) != 0 {
		t.Fatalf("declined steps ran %v", cmd2.calls)
	}
	if _, err := os.Stat(l2.BinPath); err == nil {
		t.Fatal("declined bin step ran")
	}
}

func TestUninstallRestoresEverything(t *testing.T) {
	in, cmd, tm, l := setup(t)
	in.Install(nil)
	tm.options["status-right"] = "#{@cccp_status} …"
	tm.keys["root S-NPage"] = "bind-key -T root S-NPage run-shell cccp jump"
	tm.keys["root S-PPage"] = "bind-key -T root S-PPage display-popup cccp"
	// Installed in the personal profile only.
	cmd.outputs["env -u CLAUDE_CONFIG_DIR claude plugin list"] = "cccp@cccp"
	cmd.outputs["env -u CLAUDE_CONFIG_DIR claude plugin marketplace list"] = l.MarketplaceDir
	cmd.calls = nil
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	conf, _ := os.ReadFile(l.TmuxConf)
	if string(conf) != "set -g mouse on\n" {
		t.Fatalf("tmux.conf %q", conf)
	}
	if tm.options["status-right"] != "it's #H" || tm.keys["root S-PPage"] != "bind-key -T root S-PPage copy-mode -u" {
		t.Fatalf("not restored: %q %q", tm.options["status-right"], tm.keys["root S-PPage"])
	}
	if _, ok := tm.keys["root S-NPage"]; ok {
		t.Fatal("S-NPage should be unbound again")
	}
	for _, p := range []string{l.AgyPluginDir, l.SystemdUnit, l.MarketplaceDir, l.TmuxRestore} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s left behind", p)
		}
	}
	personal := "env -u CLAUDE_CONFIG_DIR "
	for _, want := range []string{personal + "claude plugin uninstall cccp@cccp", personal + "claude plugin marketplace remove cccp", "systemctl --user disable --now cccp.service"} {
		found := false
		for _, c := range cmd.calls {
			found = found || c == want
		}
		if !found {
			t.Errorf("did not run %q (ran %v)", want, cmd.calls)
		}
	}
	hooks := 0
	for _, c := range tm.calls {
		if strings.HasPrefix(c, "set-hook -gu ") && strings.HasSuffix(c, "[97]") {
			hooks++
		}
	}
	if hooks != len(Hooks) {
		t.Fatalf("unset %d of %d hooks", hooks, len(Hooks))
	}
}

func TestReinstallMigratesKeysFromPrefixToRoot(t *testing.T) {
	in, _, tm, l := setup(t)
	// State left by an install that bound prefix a / prefix A: the saved
	// file records them by bare name (both unbound before cccp), and the
	// live server still has cccp's bindings.
	os.MkdirAll(filepath.Dir(l.TmuxRestore), 0o755)
	os.WriteFile(l.TmuxRestore, []byte(`{"options":{"status-right":"it's #H"},"keys":{"a":"","A":""}}`), 0o644)
	tm.keys["prefix a"] = "bind-key -T prefix a run-shell cccp jump"
	tm.keys["prefix A"] = "bind-key -T prefix A display-popup cccp"
	tm.options["status-right"] = "#{@cccp_status} …"
	if err := in.Install(map[string]bool{StepTmux: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := tm.keys["prefix a"]; ok {
		t.Fatal("old prefix a binding not removed")
	}
	if _, ok := tm.keys["prefix A"]; ok {
		t.Fatal("old prefix A binding not removed")
	}
	var saved Saved
	b, _ := os.ReadFile(l.TmuxRestore)
	json.Unmarshal(b, &saved)
	want := map[string]string{"root S-NPage": "", "root S-PPage": "bind-key -T root S-PPage copy-mode -u", "root S-Up": ""}
	if !reflect.DeepEqual(saved.Keys, want) {
		t.Fatalf("saved keys %v", saved.Keys)
	}
	if saved.Options["status-right"] != "it's #H" {
		t.Fatal("original options lost in the migration")
	}
}

func TestParseSavedKey(t *testing.T) {
	if k := parseSavedKey("root S-NPage"); k != (Key{"root", "S-NPage"}) {
		t.Fatal(k)
	}
	if k := parseSavedKey("a"); k != (Key{"prefix", "a"}) {
		t.Fatal("legacy bare keys are prefix-table keys:", k)
	}
}

func TestUninstallOnlyTouchesProfilesWithThePlugin(t *testing.T) {
	in, cmd, _, l := setup(t)
	in.Install(nil)
	cmd.outputs["env -u CLAUDE_CONFIG_DIR claude plugin list"] = "cccp@cccp"
	cmd.calls = nil
	in.Uninstall()
	work := "CLAUDE_CONFIG_DIR=" + filepath.Join(l.Home, ".claude-work") + " claude plugin uninstall"
	for _, c := range cmd.calls {
		if strings.HasPrefix(c, work) {
			t.Fatalf("uninstalled from a profile without the plugin: %q", c)
		}
	}
}

func TestInstallNamesClientsThatDoNotReportFocus(t *testing.T) {
	line := func(name, sess, created string) string {
		return strings.Join([]string{name, sess, "%1", "attached,focused", created, "1", "2000"}, "␟")
	}
	for _, c := range []struct {
		clients string
		advice  bool
	}{
		{line("/dev/pts/0", "alpha", "1000") + "\n" + line("/dev/pts/2", "beta", "3000") + "\n", true},
		{line("/dev/pts/2", "beta", "3000") + "\n", false},
	} {
		in, _, tm, _ := setup(t)
		var out strings.Builder
		in.Out = &out
		tm.clients = c.clients
		if err := in.Install(map[string]bool{"tmux": true}); err != nil {
			t.Fatal(err)
		}
		got := out.String()
		if c.advice != strings.Contains(got, "reattach") {
			t.Fatalf("advice %v: %q", c.advice, got)
		}
		if c.advice && (!strings.Contains(got, "/dev/pts/0 (session alpha)") || strings.Contains(got, "/dev/pts/2")) {
			t.Fatalf("must name exactly the stale client: %q", got)
		}
	}
}

func TestUninstallForgetsFocusSince(t *testing.T) {
	in, _, tm, _ := setup(t)
	in.Install(nil)
	tm.calls = nil
	in.Uninstall()
	for _, c := range tm.calls {
		if c == "set-option -gqu @cccp_focus_since" {
			return
		}
	}
	t.Fatalf("@cccp_focus_since left set: %v", tm.calls)
}
