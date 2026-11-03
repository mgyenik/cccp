// Package config loads ~/.config/cccp/config.toml.
package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the user configuration. Every field has a default.
type Config struct {
	// Notify sends a desktop notification when a session newly needs you
	// and its pane is not visible on a focused tmux client.
	Notify bool `toml:"notify"`
	// NotifyCommand is the notifier argv; the title and body are appended.
	NotifyCommand []string `toml:"notify_command"`

	// Summaries: "off", "ac" (only on mains power) or "always".
	Summaries string `toml:"summaries"`
	// SummaryModel is passed to `claude -p --model`.
	SummaryModel string `toml:"summary_model"`

	// DefaultCacheTTL is assumed when a transcript does not reveal the
	// session's cache bucket.
	DefaultCacheTTL Duration `toml:"default_cache_ttl"`
	// EndedRetention is how long ended sessions are kept (listed in the
	// ENDED group, and their state files on disk).
	EndedRetention Duration `toml:"ended_retention"`

	// QuietAfter: an unmanaged session whose pane has produced no output for
	// this long while a tool call is pending is inferred to be waiting for
	// approval.
	QuietAfter Duration `toml:"quiet_after"`

	// Profiles name Claude config directories and set per-profile policy.
	// Profiles are also discovered without configuration (from running
	// sessions and ~/.claude* directories); configuration only adds names
	// and overrides.
	Profiles []Profile `toml:"profile"`
}

// Profile configures one Claude config directory.
type Profile struct {
	Name string `toml:"name"`
	// Dir is the CLAUDE_CONFIG_DIR, "~/" allowed.
	Dir string `toml:"dir"`
	// Summaries overrides the global policy ("" = inherit).
	Summaries string `toml:"summaries"`
}

// Duration is a time.Duration that parses from a TOML string like "90m".
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.Duration.String()), nil }

// Default returns the default configuration.
func Default() Config {
	return Config{
		Notify:          true,
		NotifyCommand:   []string{"notify-send", "--app-name=cccp", "--expire-time=10000"},
		Summaries:       "ac",
		SummaryModel:    "haiku",
		DefaultCacheTTL: Duration{time.Hour},
		EndedRetention:  Duration{7 * 24 * time.Hour},
		QuietAfter:      Duration{10 * time.Second},
	}
}

func validSummaries(s string) bool { return s == "off" || s == "ac" || s == "always" }

// Parse reads a TOML configuration over the defaults. On error the defaults
// are returned with the error.
func Parse(data []byte) (Config, error) {
	c := Default()
	if _, err := toml.Decode(string(data), &c); err != nil {
		return Default(), err
	}
	if !validSummaries(c.Summaries) {
		return Default(), errors.New("summaries must be off, ac or always")
	}
	seen := map[string]bool{}
	for _, p := range c.Profiles {
		switch {
		case p.Dir == "":
			return Default(), errors.New("every [[profile]] needs a dir")
		case p.Summaries != "" && !validSummaries(p.Summaries):
			return Default(), fmt.Errorf("profile %q: summaries must be off, ac or always", p.Dir)
		case p.Name != "" && seen[p.Name]:
			return Default(), fmt.Errorf("profile name %q used twice", p.Name)
		}
		seen[p.Name] = p.Name != ""
	}
	return c, nil
}
