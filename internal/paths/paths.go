// Package paths centralises every filesystem location cccp reads or writes.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

func home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(home(), fallback)
}

// StateDir holds cccp's persistent state (hook-written session files, daemon state).
func StateDir() string {
	if v := os.Getenv("CCCP_STATE_DIR"); v != "" {
		return v
	}
	return filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), "cccp")
}

// SessionsDir holds one JSON file per hook-observed session.
func SessionsDir() string { return filepath.Join(StateDir(), "sessions") }

// SessionsLock is the lock file serialising hook read-modify-write cycles.
func SessionsLock() string { return filepath.Join(SessionsDir(), ".lock") }

// DaemonStateFile holds seen-times and aliases.
func DaemonStateFile() string { return filepath.Join(StateDir(), "daemon.json") }

// LogFile is the daemon's log.
func LogFile() string { return filepath.Join(StateDir(), "daemon.log") }

// CacheDir holds derived, rebuildable data (extracted transcript text).
func CacheDir() string {
	if v := os.Getenv("CCCP_CACHE_DIR"); v != "" {
		return v
	}
	return filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "cccp")
}

// ConfigFile is the user's config.
func ConfigFile() string {
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "cccp", "config.toml")
}

// Socket is the daemon's unix socket.
func Socket() string {
	if v := os.Getenv("CCCP_SOCKET"); v != "" {
		return v
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		return filepath.Join(rt, "cccp.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("cccp-%d.sock", os.Getuid()))
}

// ClaudeDir is Claude Code's config directory.
func ClaudeDir() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	return filepath.Join(home(), ".claude")
}

// ClaudeRegistryDir holds Claude Code's live session registry (<pid>.json).
func ClaudeRegistryDir() string { return filepath.Join(ClaudeDir(), "sessions") }

// ClaudeProjectsDir holds Claude Code transcripts, one directory per cwd.
func ClaudeProjectsDir() string { return filepath.Join(ClaudeDir(), "projects") }

var slugRe = regexp.MustCompile(`[^A-Za-z0-9]`)

// ClaudeProjectDir returns the transcript directory Claude Code uses for cwd.
// Verified against existing directories: every non-alphanumeric byte becomes '-'.
func ClaudeProjectDir(cwd string) string {
	return filepath.Join(ClaudeProjectsDir(), slugRe.ReplaceAllString(cwd, "-"))
}

// AgyDir is the Antigravity CLI data directory.
func AgyDir() string { return filepath.Join(home(), ".gemini", "antigravity-cli") }

// AgySummariesDB is agy's conversation summary database.
func AgySummariesDB() string { return filepath.Join(AgyDir(), "conversation_summaries.db") }

// AgyPluginDir is where the cccp agy plugin is installed.
func AgyPluginDir() string { return filepath.Join(home(), ".gemini", "config", "plugins", "cccp") }

// Home returns the user's home directory.
func Home() string { return home() }
