// Package present formats sessions for terminals and the tmux status line.
package present

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"cccp/internal/core"
	"cccp/internal/model"
)

// Glyph is the one-character marker for a session's group.
func Glyph(s core.Session) string {
	switch s.Group {
	case core.GroupNeedsYou:
		if s.Activity == model.Errored {
			return "✗"
		}
		return "⚠"
	case core.GroupYourTurn:
		return "⏎"
	case core.GroupWorking:
		return "●"
	case core.GroupIdle:
		return "○"
	case core.GroupCold:
		return "❄"
	case core.GroupUnknown:
		return "?"
	case core.GroupClosed:
		return "·"
	}
	return " "
}

// GroupGlyph is the glyph for a group.
func GroupGlyph(g core.Group) string {
	return Glyph(core.Session{Group: g})
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
func CacheText(s core.Session, now time.Time) string {
	if !s.Cache.Known || s.Group == core.GroupWorking || s.Group == core.GroupClosed {
		return ""
	}
	suffix := ""
	if s.Cache.TTLAssumed || s.Cache.Fades {
		suffix = "?"
	}
	switch {
	case s.Cache.Fades && s.Cache.Cooling(now):
		return "cooling?" // a best-effort cache has no deadline to count down to
	case s.Cache.Fades && s.Cache.Warm(now):
		return "warm?"
	case s.Cache.Warm(now):
		return "warm " + Age(s.Cache.ExpiresAt.Sub(now)) + suffix
	}
	return "cold " + Age(now.Sub(s.Cache.ExpiresAt)) + suffix
}

// Turns formats a conversation's length: "1 turn", "12 turns", "" when
// unknown.
func Turns(n int) string {
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "1 turn"
	}
	return fmt.Sprintf("%d turns", n)
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
func Detail(s core.Session) string {
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
	case s.Summary != "" && (s.Group == core.GroupYourTurn || s.Group == core.GroupNeedsYou):
		return s.Summary
	case s.Group == core.GroupYourTurn && s.LastMessage != "":
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

// Around returns the snippet s on one line, starting at most lead runes
// (rounded to a word start) before the earliest occurrence of any of terms,
// so the match stays visible in a narrow column that truncates on the
// right. Matching is case-insensitive; with no occurrence s is unchanged.
func Around(s string, terms []string, lead int) string {
	cut := strings.HasPrefix(s, "…")
	rs := []rune(OneLine(strings.TrimPrefix(s, "…")))
	low := make([]rune, len(rs))
	for i, r := range rs {
		low[i] = unicode.ToLower(r)
	}
	at := -1
	for _, t := range terms {
		if i := indexRunes(low, []rune(strings.ToLower(t))); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	start := 0
	if at > lead {
		start = at - lead
		for j := start; j < at; j++ { // begin at a word, if one starts in the lead
			if rs[j] == ' ' {
				start = j + 1
				break
			}
		}
	}
	out := string(rs[start:])
	if cut || start > 0 {
		out = "…" + out
	}
	return out
}

func indexRunes(s, sub []rune) int {
	if len(sub) == 0 {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		j := 0
		for j < len(sub) && s[i+j] == sub[j] {
			j++
		}
		if j == len(sub) {
			return i
		}
	}
	return -1
}

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
func StatusSegment(snap core.Snapshot) string {
	c := snap.Counts()
	var parts []string
	add := func(g core.Group, style string) {
		if n := c[g]; n > 0 {
			parts = append(parts, fmt.Sprintf("#[%s]%s%d#[default]", style, GroupGlyph(g), n))
		}
	}
	add(core.GroupNeedsYou, "fg=yellow,bold")
	add(core.GroupYourTurn, "fg=green,bold")
	add(core.GroupWorking, "fg=cyan")
	add(core.GroupIdle, "fg=default")
	add(core.GroupCold, "fg=blue")
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// PlainStatus renders counts without tmux styles.
func PlainStatus(snap core.Snapshot) string {
	c := snap.Counts()
	var parts []string
	for _, g := range []core.Group{core.GroupNeedsYou, core.GroupYourTurn, core.GroupWorking, core.GroupIdle, core.GroupCold, core.GroupUnknown} {
		if n := c[g]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s%d", GroupGlyph(g), n))
		}
	}
	return strings.Join(parts, " ")
}

// WindowGlyphs computes the @cccp_glyph value for every tmux window holding a
// session: the most urgent session in the window decides.
func WindowGlyphs(snap core.Snapshot) map[string]string {
	best := map[string]core.Session{}
	for _, s := range snap.Sessions {
		if s.WindowID == "" || s.Group == core.GroupClosed {
			continue
		}
		if cur, ok := best[s.WindowID]; !ok || s.Group < cur.Group {
			best[s.WindowID] = s
		}
	}
	out := map[string]string{}
	for w, s := range best {
		switch s.Group {
		case core.GroupNeedsYou:
			out[w] = "#[fg=yellow,bold]" + Glyph(s) + "#[default] "
		case core.GroupYourTurn:
			out[w] = "#[fg=green,bold]⏎#[default] "
		case core.GroupWorking:
			out[w] = "#[fg=cyan]●#[default] "
		case core.GroupCold:
			out[w] = "#[fg=blue]❄#[default] "
		case core.GroupIdle:
			out[w] = "○ "
		case core.GroupUnknown:
			out[w] = "? "
		}
	}
	return out
}

// MultiProfile reports whether sessions from more than one Claude profile
// are present, in which case profile tags are shown.
func MultiProfile(snap core.Snapshot) bool {
	seen := ""
	for _, s := range snap.Sessions {
		if s.Profile == "" {
			continue
		}
		if seen != "" && s.Profile != seen {
			return true
		}
		seen = s.Profile
	}
	return false
}

// ProfileTag is a session's profile label for display, at most n runes
// ("" for sessions without a profile, such as agy's).
func ProfileTag(s core.Session, n int) string {
	r := []rune(s.Profile)
	if len(r) > n {
		r = append(r[:n-1], '…')
	}
	return string(r)
}

// TitleWithProfile prefixes the title with "[profile] " when profiles are
// shown.
func TitleWithProfile(s core.Session, show bool) string {
	if show && s.Profile != "" {
		return "[" + s.Profile + "] " + s.DisplayTitle()
	}
	return s.DisplayTitle()
}
