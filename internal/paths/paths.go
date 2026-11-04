// Package paths resolves every location cccp uses. Resolve is a pure
// function of the environment; only the composition root (cmd/cccp) calls
// it, and everything else receives the paths it needs as values.
package paths

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// Layout is the resolved set of locations.
type Layout struct {
	Home string

	// cccp's own state, cache and config.
	StateDir        string // ~/.local/state/cccp
	SessionsDir     string // hook-written session state files
	MemoryFile      string // daemon memory (seen marks, aliases)
	SummariesFile   string // generated summaries
	WorkstreamsFile string // the workstream record (docs/workstreams.md)

	HookErrorLog string
	CacheDir     string // ~/.cache/cccp
	TextDir      string // extracted transcript text for search
	ConfigFile   string
	Socket       string
	// TmuxSocket selects a tmux server; empty means tmux's default.
	TmuxSocket string

	// DefaultClaudeDir is Claude Code's configuration directory when
	// CLAUDE_CONFIG_DIR is unset: the default profile. It is fixed (not
	// taken from cccp's own environment), because a session that reports
	// no CLAUDE_CONFIG_DIR is in exactly this directory. Other profiles are
	// configured or discovered (package profile).
	DefaultClaudeDir string

	// Antigravity CLI.
	AgyDir         string
	AgySummariesDB string
	AgyBrainDir    string
	AgyPluginDir   string

	// Installed artefacts.
	ShareDir       string // ~/.local/share/cccp
	MarketplaceDir string
	TmuxSnippet    string
	TmuxRestore    string
	BinPath        string
	SystemdUnit    string
	TmuxConf       string
}

// EnvVars are the environment variables Resolve reads. A process that starts
// cccp in a new environment (a tmux popup) passes these through so the child
// resolves the same layout.
var EnvVars = []string{
	"CCCP_STATE_DIR", "CCCP_CACHE_DIR", "CCCP_SOCKET", "CCCP_TMUX_SOCKET",
	"XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR",
}

// Resolve computes the layout from environment variables (getenv) and the
// home directory. Overrides: CCCP_STATE_DIR, CCCP_CACHE_DIR, CCCP_SOCKET,
// CCCP_TMUX_SOCKET, XDG_STATE_HOME, XDG_CACHE_HOME, XDG_CONFIG_HOME,
// XDG_RUNTIME_DIR.
func Resolve(getenv func(string) string, home string, uid int) Layout {
	xdg := func(env, fallback string) string {
		if v := getenv(env); v != "" && filepath.IsAbs(v) {
			return v
		}
		return filepath.Join(home, fallback)
	}
	or := func(v, fallback string) string {
		if v != "" {
			return v
		}
		return fallback
	}
	l := Layout{Home: home}
	l.StateDir = or(getenv("CCCP_STATE_DIR"), filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), "cccp"))
	l.SessionsDir = filepath.Join(l.StateDir, "sessions")
	l.MemoryFile = filepath.Join(l.StateDir, "daemon.json")
	l.SummariesFile = filepath.Join(l.StateDir, "summaries.json")
	l.WorkstreamsFile = filepath.Join(l.StateDir, "workstreams.json")

	l.HookErrorLog = filepath.Join(l.StateDir, "hook-errors.log")
	l.CacheDir = or(getenv("CCCP_CACHE_DIR"), filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "cccp"))
	l.TextDir = filepath.Join(l.CacheDir, "text")
	l.ConfigFile = filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "cccp", "config.toml")
	switch {
	case getenv("CCCP_SOCKET") != "":
		l.Socket = getenv("CCCP_SOCKET")
	case getenv("XDG_RUNTIME_DIR") != "":
		l.Socket = filepath.Join(getenv("XDG_RUNTIME_DIR"), "cccp.sock")
	default:
		l.Socket = filepath.Join("/tmp", fmt.Sprintf("cccp-%d.sock", uid))
	}
	l.TmuxSocket = getenv("CCCP_TMUX_SOCKET")

	l.DefaultClaudeDir = filepath.Join(home, ".claude")

	l.AgyDir = filepath.Join(home, ".gemini", "antigravity-cli")
	l.AgySummariesDB = filepath.Join(l.AgyDir, "conversation_summaries.db")
	l.AgyBrainDir = filepath.Join(l.AgyDir, "brain")
	l.AgyPluginDir = filepath.Join(home, ".gemini", "config", "plugins", "cccp")

	l.ShareDir = filepath.Join(home, ".local", "share", "cccp")
	l.MarketplaceDir = filepath.Join(l.ShareDir, "marketplace")
	l.TmuxSnippet = filepath.Join(l.ShareDir, "cccp.tmux")
	l.TmuxRestore = filepath.Join(l.ShareDir, "tmux-restore.json")
	l.BinPath = filepath.Join(home, ".local", "bin", "cccp")
	l.SystemdUnit = filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "systemd", "user", "cccp.service")
	l.TmuxConf = filepath.Join(home, ".tmux.conf")
	return l
}

var slugRe = regexp.MustCompile(`[^A-Za-z0-9]`)

// ProjectSlug is the name of the directory under ~/.claude/projects that
// Claude Code uses for cwd. Verified against existing directories: every
// non-alphanumeric byte becomes '-'.
func ProjectSlug(cwd string) string { return slugRe.ReplaceAllString(cwd, "-") }

// ClaudeProjectDir is the transcript directory for cwd in a Claude profile.
func ClaudeProjectDir(configDir, cwd string) string {
	return filepath.Join(configDir, "projects", ProjectSlug(cwd))
}

// AgyTranscript is an agy conversation's full transcript.
func (l Layout) AgyTranscript(id string) string {
	return filepath.Join(l.AgyBrainDir, id, ".system_generated", "logs", "transcript_full.jsonl")
}
