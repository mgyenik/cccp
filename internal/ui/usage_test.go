package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"cccp/internal/usage"
)

// pressRun sends a key and runs what it starts (the usage fetches).
func pressRun(m *Model, k rune) {
	_, cmd := m.Update(tea.KeyPressMsg{Code: k, Text: string(k)})
	run(m, cmd)
}

func TestUsageScreen(t *testing.T) {
	m, _, _ := newModel(t)
	m.Update(snapMsg(snapshot()))
	now := time.Now()
	m.now = now
	fetched := 0
	m.opt.Usage = func() []UsageSource {
		return []UsageSource{
			{Provider: "Claude", Name: "personal", Fetch: func() usage.Account {
				fetched++
				return usage.Account{Provider: "Claude", Name: "personal", Limits: []usage.Limit{
					{Label: "Current session", Used: 80, ResetsAt: now.Add(3*time.Hour + 18*time.Minute), Severity: "warning"},
					{Label: "Week (all models)", Used: 2, ResetsAt: now.Add(4*24*time.Hour + 2*time.Hour + 30*time.Second), Severity: "normal"},
				}}
			}},
			{Provider: "Claude", Name: "work", Fetch: func() usage.Account {
				return usage.Account{Provider: "Claude", Name: "work", Err: "claude: Not logged in · Please run /login"}
			}},
			{Provider: "Antigravity", Fetch: func() usage.Account {
				return usage.Account{Provider: "Antigravity", Limits: []usage.Limit{{Label: "Gemini · week", Used: 50}}}
			}},
		}
	}
	pressRun(m, 'u')
	if m.mode != modeUsage || fetched != 1 {
		t.Fatalf("mode %v, fetched %d", m.mode, fetched)
	}
	out := ansi.Strip(m.render())
	for _, want := range []string{
		"Subscription usage", "Claude · personal", "Current session", "80% used", "resets in 3h 18m",
		"Week (all models)", "2% used", "resets in 4d 2h", "Claude · work", "Not logged in", "Antigravity", "Gemini · week", "50% used",
		"r refresh · esc close",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// r fetches again; esc closes.
	pressRun(m, 'r')
	if fetched != 2 {
		t.Fatalf("refresh fetched %d", fetched)
	}
	key(m, "esc")
	if m.mode != modeList || m.usage != nil {
		t.Fatal("esc closes")
	}
}

// Accounts appear as they come; a result from an earlier opening is dropped.
func TestUsageScreenFillsInAndDropsStaleResults(t *testing.T) {
	m, _, _ := newModel(t)
	m.Update(snapMsg(snapshot()))
	m.opt.Usage = func() []UsageSource { return []UsageSource{{Provider: "Antigravity"}} }
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if out := ansi.Strip(m.render()); !strings.Contains(out, "finding accounts") {
		t.Fatalf("before the list:\n%s", out)
	}
	m.Update(cmd()) // the list, not yet fetched
	if out := ansi.Strip(m.render()); !strings.Contains(out, "fetching…") {
		t.Fatalf("before the fetch:\n%s", out)
	}
	old := m.usage.gen
	m.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	m.Update(usageSourcesMsg{gen: old, sources: []UsageSource{{Provider: "stale"}, {Provider: "stale"}}})
	if m.usage.listed {
		t.Fatal("an account list from before the refresh was kept")
	}
	m.Update(usageMsg{gen: old, i: 0, account: usage.Account{Limits: []usage.Limit{{Label: "stale", Used: 1}}}})
	if len(m.usage.accounts) != 0 {
		t.Fatal("a result from before the refresh was kept")
	}
	// Without a usage source: a message, no screen.
	m.mode, m.usage, m.opt.Usage = modeList, nil, nil
	key(m, "u")
	if m.mode != modeList || !strings.Contains(m.status, "not available") {
		t.Fatalf("mode %v status %q", m.mode, m.status)
	}
}

func TestUsageMeterAndResets(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		l    usage.Limit
		want string // the bar's colour
	}{
		{usage.Limit{Used: 95, Severity: "normal"}, "32"}, // the server's reading wins
		{usage.Limit{Used: 10, Severity: "critical"}, "31"},
		{usage.Limit{Used: 10, Severity: "warning"}, "33"},
		{usage.Limit{Used: 95}, "31"}, // no severity (agy): by share
		{usage.Limit{Used: 80}, "33"},
		{usage.Limit{Used: 10}, "32"},
	} {
		if got := usageMeter(c.l); !strings.Contains(got, "\x1b[3"+c.want[1:]+"m") && !strings.Contains(got, ";"+c.want+"m") && !strings.Contains(got, "["+c.want+"m") {
			t.Errorf("%+v: %q, want colour %s", c.l, got, c.want)
		}
	}
	if got := ansi.Strip(usageMeter(usage.Limit{Used: 50})); got != strings.Repeat("█", 10)+strings.Repeat("░", 10)+"  50% used" {
		t.Fatalf("%q", got)
	}
	if got := ansi.Strip(usageMeter(usage.Limit{Used: 130})); !strings.HasPrefix(got, strings.Repeat("█", 20)) || !strings.Contains(got, "100%") {
		t.Fatalf("clamped: %q", got)
	}
	for at, want := range map[time.Time]string{
		{}:                                    "",
		now.Add(-time.Minute):                 "reset",
		now.Add(7*time.Minute + time.Second):  "resets in 8m",
		now.Add(3*time.Hour + 18*time.Minute): "resets in 3h 18m",
		now.Add(50 * time.Hour):               "resets in 2d 2h",
	} {
		if got := resetText(now, at); !strings.HasPrefix(got, want) || ((want == "" || want == "reset") && got != want) {
			t.Errorf("%v: %q want %q", at, got, want)
		}
	}
}
