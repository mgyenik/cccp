// Package present formats sessions for terminals and the tmux status line.
package present

import (
	"fmt"
	"strings"
	"time"

	"cccp/internal/engine"
	"cccp/internal/model"
)

// Glyph is the one-character marker for a session's group.
func Glyph(s engine.Session) string {
	switch s.Group {
	case engine.GroupNeedsYou:
		if s.Activity == model.Errored {
			return "✗"
		}
		return "⚠"
	case engine.GroupYourTurn:
		return "⏎"
	case engine.GroupWorking:
		return "●"
	case engine.GroupIdle:
		return "○"
	case engine.GroupCold:
		return "❄"
	case engine.GroupUnknown:
		return "?"
	case engine.GroupEnded:
		return "·"
	}
	return " "
}

// GroupGlyph is the glyph for a group.
func GroupGlyph(g engine.Group) string {
	return Glyph(engine.Session{Group: g})
}

// AgentBadge is a two-letter agent marker.
func AgentBadge(a model.Agent) string {
	switch a {
	case model.Claude:
		return "CC"
	case model.Agy:
		return "AG"
	}
	return "??"
}

// Age formats a duration compactly: 45s, 12m, 3h, 2d.
func Age(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Since formats the time since t, or "" when t is zero.
func Since(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return Age(now.Sub(t))
}

// CacheText describes the cache state: "warm 42m", "cold 3h", or "".
func CacheText(s engine.Session, now time.Time) string {
	if !s.Cache.Known || s.Group == engine.GroupWorking || s.Group == engine.GroupEnded {
		return ""
	}
	suffix := ""
	if s.Cache.TTLAssumed {
		suffix = "?"
	}
	if s.Cache.Warm(now) {
		return "warm " + Age(s.Cache.ExpiresAt.Sub(now)) + suffix
	}
	return "cold " + Age(now.Sub(s.Cache.ExpiresAt)) + suffix
}

// Tokens formats a token count: 254k, 1.2M.
func Tokens(n int) string {
	switch {
	case n <= 0:
		return ""
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%.1fM", float64(n)/1e6)
}

// Detail is the most useful one-line description of what a session is doing
// or waiting on.
func Detail(s engine.Session) string {
	switch {
	case s.Activity == model.Errored && s.Error != "":
		return "error: " + s.Error
	case s.Question != "":
		return s.Question
	case s.Activity == model.Working && s.Tool != "":
		if s.ToolDetail != "" {
			return s.Tool + ": " + s.ToolDetail
		}
		return s.Tool
	case s.Summary != "" && (s.Group == engine.GroupYourTurn || s.Group == engine.GroupNeedsYou):
		return s.Summary
	case s.Group == engine.GroupYourTurn && s.LastMessage != "":
		return LastLine(s.LastMessage)
	case s.LastPrompt != "":
		return "› " + OneLine(s.LastPrompt)
	}
	if len(s.Notes) > 0 {
		return s.Notes[0]
	}
	return ""
}

// OneLine collapses whitespace.
func OneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// LastLine returns the last non-empty paragraph line of a message, which is
// usually the question or the conclusion.
func LastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "```") {
			return OneLine(l)
		}
	}
	return ""
}

// StatusSegment renders counts for the tmux status line, with tmux style
// sequences. Empty groups are omitted; an empty string means nothing to show.
func StatusSegment(snap engine.Snapshot) string {
	c := snap.Counts()
	var parts []string
	add := func(g engine.Group, style string) {
		if n := c[g]; n > 0 {
			parts = append(parts, fmt.Sprintf("#[%s]%s%d#[default]", style, GroupGlyph(g), n))
		}
	}
	add(engine.GroupNeedsYou, "fg=yellow,bold")
	add(engine.GroupYourTurn, "fg=green,bold")
	add(engine.GroupWorking, "fg=cyan")
	add(engine.GroupIdle, "fg=default")
	add(engine.GroupCold, "fg=blue")
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// PlainStatus renders counts without tmux styles.
func PlainStatus(snap engine.Snapshot) string {
	c := snap.Counts()
	var parts []string
	for _, g := range []engine.Group{engine.GroupNeedsYou, engine.GroupYourTurn, engine.GroupWorking, engine.GroupIdle, engine.GroupCold, engine.GroupUnknown} {
		if n := c[g]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s%d", GroupGlyph(g), n))
		}
	}
	return strings.Join(parts, " ")
}

// WindowGlyphs computes the @cccp_glyph value for every tmux window holding a
// session: the most urgent session in the window decides.
func WindowGlyphs(snap engine.Snapshot) map[string]string {
	best := map[string]engine.Session{}
	for _, s := range snap.Sessions {
		if s.WindowID == "" || s.Group == engine.GroupEnded {
			continue
		}
		if cur, ok := best[s.WindowID]; !ok || s.Group < cur.Group {
			best[s.WindowID] = s
		}
	}
	out := map[string]string{}
	for w, s := range best {
		switch s.Group {
		case engine.GroupNeedsYou:
			out[w] = "#[fg=yellow,bold]" + Glyph(s) + "#[default] "
		case engine.GroupYourTurn:
			out[w] = "#[fg=green,bold]⏎#[default] "
		case engine.GroupWorking:
			out[w] = "#[fg=cyan]●#[default] "
		case engine.GroupCold:
			out[w] = "#[fg=blue]❄#[default] "
		case engine.GroupIdle:
			out[w] = "○ "
		case engine.GroupUnknown:
			out[w] = "? "
		}
	}
	return out
}
