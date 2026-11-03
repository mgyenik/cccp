package model

import (
	"path/filepath"
	"strings"
)

// Profile is a Claude Code configuration directory (CLAUDE_CONFIG_DIR):
// its own sessions, transcripts, plugins and account.
type Profile struct {
	Name string // short label, e.g. "work"
	Dir  string // absolute config directory, cleaned
	// Default: the profile used when CLAUDE_CONFIG_DIR is unset.
	Default bool
	// Summaries overrides the global summaries policy for this profile
	// ("" = inherit).
	Summaries string
}

// RegistryDir is the profile's live session registry.
func (p Profile) RegistryDir() string { return filepath.Join(p.Dir, "sessions") }

// ProjectsDir holds the profile's transcripts, one directory per cwd.
func (p Profile) ProjectsDir() string { return filepath.Join(p.Dir, "projects") }

// ConfigDirEnv is the variable that selects a Claude profile.
const ConfigDirEnv = "CLAUDE_CONFIG_DIR"

// Environ returns environ with p selected for a claude child process.
//
// The default profile is selected by removing CLAUDE_CONFIG_DIR, never by
// setting it to ~/.claude: the two are not the same. Claude Code keeps its
// global state (onboarding, theme, per-project trust) in ~/.claude.json
// when the variable is unset, but in $CLAUDE_CONFIG_DIR/.claude.json when
// it is set, so CLAUDE_CONFIG_DIR=~/.claude starts from a blank state and
// runs the first-start flow again. Any other profile sets it to Dir.
func (p Profile) Environ(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, ConfigDirEnv+"=") {
			out = append(out, kv)
		}
	}
	if !p.Default {
		out = append(out, ConfigDirEnv+"="+p.Dir)
	}
	return out
}
