// Package config loads ~/.config/cccp/config.toml.
package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/BurntSushi/toml"

	"cccp/internal/jail"
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

	// Jail sets the memory and CPU limits of tmux and its workstreams
	// (docs/jails.md).
	Jail Jail `toml:"jail"`
}

// Jail is the [jail] section: on/off, changes to the built-in limits
// (jail.Defaults), and workstreams' own settings.
type Jail struct {
	Enabled bool `toml:"enabled"`
	// Root and RootMax change A's limits (all of tmux), normally and
	// while a workstream is at max.
	Root    jail.Limits `toml:"root"`
	RootMax jail.Limits `toml:"root_max"`
	// Presets change a preset's limits ([jail.preset.boost]).
	Presets map[string]jail.Limits `toml:"preset"`
	// Workstreams set a workstream's normal limits ([[jail.workstream]]).
	Workstreams []JailWorkstream `toml:"workstream"`
}

// JailWorkstream is one workstream's normal limits: a preset, and/or
// values of its own over it.
type JailWorkstream struct {
	Name   string `toml:"name"`
	Preset string `toml:"preset"`
	jail.Limits
}

// Settings are the limits in force: the built-in ones changed by j.
func (j Jail) Settings() jail.Settings {
	s := jail.Settings{
		Root:        jail.Defaults.Root.Merge(j.Root),
		RootMax:     jail.Defaults.RootMax.Merge(j.RootMax),
		Presets:     map[string]jail.Limits{},
		Workstreams: map[string]jail.WorkstreamSetting{},
	}
	for name, l := range jail.Defaults.Presets {
		s.Presets[name] = l.Merge(j.Presets[name])
	}
	for _, w := range j.Workstreams {
		s.Workstreams[w.Name] = jail.WorkstreamSetting{Preset: w.Preset, Custom: w.Limits}
	}
	return s
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
		Jail:            Jail{Enabled: true},
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
	if err := c.Jail.validate(); err != nil {
		return Default(), err
	}
	return c, nil
}

// validate checks every limit encodes for systemd and every preset named
// exists, so a mistake is reported when the config loads.
func (j Jail) validate() error {
	known := map[string]bool{}
	for _, p := range jail.Presets {
		known[p] = true
	}
	check := func(what string, l jail.Limits) error {
		if _, err := jail.Properties(l); err != nil {
			return fmt.Errorf("jail %s: %v", what, err)
		}
		return nil
	}
	if err := check("root", j.Root); err != nil {
		return err
	}
	if err := check("root_max", j.RootMax); err != nil {
		return err
	}
	for name, l := range j.Presets {
		if !known[name] {
			return fmt.Errorf("jail preset %q: the presets are %v", name, jail.Presets)
		}
		if err := check("preset "+name, l); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, w := range j.Workstreams {
		switch {
		case w.Name == "":
			return errors.New("every [[jail.workstream]] needs a name")
		case seen[w.Name]:
			return fmt.Errorf("jail workstream %q set twice", w.Name)
		case w.Preset != "" && !known[w.Preset]:
			return fmt.Errorf("jail workstream %q: preset %q: the presets are %v", w.Name, w.Preset, jail.Presets)
		}
		seen[w.Name] = true
		if err := check("workstream "+w.Name, w.Limits); err != nil {
			return err
		}
	}
	return nil
}
