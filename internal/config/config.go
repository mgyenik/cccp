// Package config loads ~/.config/cccp/config.toml.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"

	"cccp/internal/paths"
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

// Load reads the config file over the defaults. A missing file is not an error.
func Load() (Config, error) {
	c := Default()
	b, err := os.ReadFile(paths.ConfigFile())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if _, err := toml.Decode(string(b), &c); err != nil {
		return Default(), fmt.Errorf("%s: %w", paths.ConfigFile(), err)
	}
	switch c.Summaries {
	case "off", "ac", "always":
	default:
		return Default(), fmt.Errorf("%s: summaries must be off, ac or always", paths.ConfigFile())
	}
	return c, nil
}
