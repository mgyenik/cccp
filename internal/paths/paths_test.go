package paths

import "testing"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveDefaults(t *testing.T) {
	l := Resolve(env(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1000"}), "/h", 1000)
	want := map[string]string{
		"state":    "/h/.local/state/cccp",
		"sessions": "/h/.local/state/cccp/sessions",
		"cache":    "/h/.cache/cccp",
		"config":   "/h/.config/cccp/config.toml",
		"socket":   "/run/user/1000/cccp.sock",
		"claude":   "/h/.claude",
		"registry": "/h/.claude/sessions",
		"projects": "/h/.claude/projects",
		"agydb":    "/h/.gemini/antigravity-cli/conversation_summaries.db",
		"bin":      "/h/.local/bin/cccp",
		"unit":     "/h/.config/systemd/user/cccp.service",
	}
	got := map[string]string{
		"state": l.StateDir, "sessions": l.SessionsDir, "cache": l.CacheDir, "config": l.ConfigFile,
		"socket": l.Socket, "claude": l.ClaudeDir, "registry": l.ClaudeRegistryDir, "projects": l.ClaudeProjectsDir,
		"agydb": l.AgySummariesDB, "bin": l.BinPath, "unit": l.SystemdUnit,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
	if l.TmuxSocket != "" {
		t.Errorf("tmux socket %q", l.TmuxSocket)
	}
}

func TestResolveOverrides(t *testing.T) {
	l := Resolve(env(map[string]string{
		"CCCP_STATE_DIR": "/s", "CCCP_CACHE_DIR": "/c", "CCCP_SOCKET": "/x.sock",
		"CCCP_TMUX_SOCKET": "/t", "CLAUDE_CONFIG_DIR": "/cl", "XDG_CONFIG_HOME": "/cfg",
		"XDG_STATE_HOME": "relative/is/ignored",
	}), "/h", 1)
	if l.StateDir != "/s" || l.CacheDir != "/c" || l.Socket != "/x.sock" || l.TmuxSocket != "/t" ||
		l.ClaudeRegistryDir != "/cl/sessions" || l.ConfigFile != "/cfg/cccp/config.toml" {
		t.Fatalf("%+v", l)
	}
	if l := Resolve(env(nil), "/h", 7); l.Socket != "/tmp/cccp-7.sock" {
		t.Fatalf("socket fallback %q", l.Socket)
	}
}

func TestProjectSlug(t *testing.T) {
	// Real directory names observed under ~/.claude/projects.
	for cwd, want := range map[string]string{
		"/home/user/src/cccp":              "-home-user-src-cccp",
		"/home/user/src/synth/osc_bank/v2": "-home-user-src-synth-osc-bank-v2",
		"/home/user/scratch/led-tests":     "-home-user-scratch-led-tests",
	} {
		if got := ProjectSlug(cwd); got != want {
			t.Errorf("%q → %q want %q", cwd, got, want)
		}
	}
}
