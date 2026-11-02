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

func (f *fakeCmd) Run(name string, args ...string) (string, error) {
	c := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, c)
	return f.outputs[c], nil
}

type fakeTmux struct {
	options map[string]string
	keys    map[string]string
	calls   []string
}

func (f *fakeTmux) Run(args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case len(args) == 3 && args[0] == "show-options":
		return f.options[args[2]] + "\n", nil
	case len(args) == 4 && args[0] == "list-keys":
		return f.keys[args[3]], nil
	case len(args) == 4 && args[0] == "set-option" && args[1] == "-g":
		f.options[args[2]] = args[3]
	case len(args) == 4 && args[0] == "unbind-key": // unbind-key -T prefix KEY
		delete(f.keys, args[3])
	case len(args) == 2 && args[0] == "source-file" && strings.HasSuffix(args[1], ".keys"):
		b, _ := os.ReadFile(args[1])
		f.keys["a"] = strings.TrimSpace(string(b))
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
		keys: map[string]string{"a": "bind-key -T prefix a display-message hi"},
	}
	return &Installer{L: l, Assets: cccp.Assets, Cmd: cmd, Tmux: tm, Self: self, Out: io.Discard,
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
	wantCmds := []string{
		"claude plugin marketplace list",
		"claude plugin marketplace add " + l.MarketplaceDir,
		"claude plugin list",
		"claude plugin install cccp@cccp",
		"systemctl --user daemon-reload",
		"systemctl --user enable cccp.service",
		"systemctl --user restart cccp.service",
	}
	if !reflect.DeepEqual(cmd.calls, wantCmds) {
		t.Fatalf("commands:\n%s", strings.Join(cmd.calls, "\n"))
	}
	conf, _ := os.ReadFile(l.TmuxConf)
	if !strings.Contains(string(conf), SourceLine(l.TmuxSnippet)) {
		t.Fatalf("tmux.conf %q", conf)
	}
	if tm.calls[len(tm.calls)-1] != "source-file "+l.TmuxSnippet {
		t.Fatalf("snippet not sourced: %v", tm.calls)
	}
	var saved Saved
	b, _ := os.ReadFile(l.TmuxRestore)
	json.Unmarshal(b, &saved)
	if saved.Options["status-right"] != "it's #H" || saved.Keys["a"] == "" || saved.Keys["A"] != "" {
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
	tm.keys["a"] = "bind-key -T prefix a run-shell cccp jump"
	tm.keys["A"] = "bind-key -T prefix A display-popup cccp"
	cmd.outputs["claude plugin list"] = "cccp@cccp"
	cmd.outputs["claude plugin marketplace list"] = l.MarketplaceDir
	cmd.calls = nil
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	conf, _ := os.ReadFile(l.TmuxConf)
	if string(conf) != "set -g mouse on\n" {
		t.Fatalf("tmux.conf %q", conf)
	}
	if tm.options["status-right"] != "it's #H" || tm.keys["a"] != "bind-key -T prefix a display-message hi" {
		t.Fatalf("not restored: %q %q", tm.options["status-right"], tm.keys["a"])
	}
	if _, ok := tm.keys["A"]; ok {
		t.Fatal("prefix A should be unbound again")
	}
	for _, p := range []string{l.AgyPluginDir, l.SystemdUnit, l.MarketplaceDir, l.TmuxRestore} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s left behind", p)
		}
	}
	for _, want := range []string{"claude plugin uninstall cccp@cccp", "claude plugin marketplace remove cccp", "systemctl --user disable --now cccp.service"} {
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
