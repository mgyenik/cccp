package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"cccp/internal/usage"
)

// UsageSource is one account whose subscription usage the cockpit can show
// (u). Fetch runs in the background, once per opening or refresh.
type UsageSource struct {
	Provider string // "Claude", "Antigravity"
	Name     string // the Claude profile ("" for agy)
	Fetch    func() usage.Account
}

// usageView is the usage screen: the accounts in order, each filled in as
// its fetch returns.
type usageView struct {
	gen       int // results of an earlier opening or refresh are dropped
	listed    bool
	sources   []UsageSource
	accounts  map[int]usage.Account
	fetchedAt time.Time
}

type usageSourcesMsg struct {
	gen     int
	sources []UsageSource
}

type usageMsg struct {
	gen, i  int
	account usage.Account
}

// openUsage starts (or restarts) a fetch of every account.
func (m *Model) openUsage() tea.Cmd {
	m.usageGen++
	gen, list := m.usageGen, m.opt.Usage
	m.usage = &usageView{gen: gen, accounts: map[int]usage.Account{}, fetchedAt: m.now}
	m.mode = modeUsage
	return func() tea.Msg { return usageSourcesMsg{gen: gen, sources: list()} }
}

// usageSources records the accounts and fetches each.
func (m *Model) usageSources(msg usageSourcesMsg) tea.Cmd {
	if m.usage == nil || msg.gen != m.usage.gen {
		return nil
	}
	m.usage.sources, m.usage.listed = msg.sources, true
	var cmds []tea.Cmd
	for i, s := range msg.sources {
		i, fetch, gen := i, s.Fetch, msg.gen
		cmds = append(cmds, func() tea.Msg { return usageMsg{gen: gen, i: i, account: fetch()} })
	}
	return tea.Batch(cmds...)
}

func (m *Model) usageResult(msg usageMsg) {
	if m.usage != nil && msg.gen == m.usage.gen {
		m.usage.accounts[msg.i] = msg.account
	}
}

// usageKey handles a key on the usage screen.
func (m *Model) usageKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc", "q", "u":
		m.mode, m.usage = modeList, nil
	case "r":
		return m.openUsage()
	}
	return nil
}

const usageBar = 20

func (v *usageView) view(now time.Time, w, h int) string {
	lines := []string{sTitle.Render("Subscription usage") + sDim.Render("  as of "+v.fetchedAt.Format("15:04")), ""}
	if !v.listed {
		lines = append(lines, sDim.Render("  finding accounts…"))
	}
	if v.listed && len(v.sources) == 0 {
		lines = append(lines, sDim.Render("  no Claude profile or agy found"))
	}
	labelW := 0
	for _, a := range v.accounts {
		for _, l := range a.Limits {
			labelW = max(labelW, ansi.StringWidth(l.Label))
		}
	}
	for i, s := range v.sources {
		head := s.Provider
		if s.Name != "" {
			head += " · " + s.Name
		}
		lines = append(lines, sTitle.Render(head))
		a, ok := v.accounts[i]
		switch {
		case !ok:
			lines = append(lines, sDim.Render("  fetching…"))
		case a.Err != "":
			lines = append(lines, lipgloss.NewStyle().Foreground(cRed).Render("  "+a.Err))
		case len(a.Limits) == 0 && a.Note == "":
			lines = append(lines, sDim.Render("  no limits reported"))
		}
		for _, l := range a.Limits {
			lines = append(lines, "  "+pad(l.Label, labelW)+"  "+usageMeter(l)+"  "+sDim.Render(resetText(now, l.ResetsAt)))
		}
		if a.Note != "" {
			lines = append(lines, sDim.Render("  "+a.Note))
		}
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], w, "…")
	}
	return lipgloss.NewStyle().Height(h).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

// usageMeter is a bar and the share used, coloured by the server's
// severity when it gives one (Claude), else by the share (agy gives none).
func usageMeter(l usage.Limit) string {
	used := math.Max(0, math.Min(100, l.Used))
	n := int(math.Round(used / 100 * usageBar))
	c := cGreen
	switch {
	case l.Severity == "critical", l.Severity == "" && used >= 90:
		c = cRed
	case l.Severity == "warning", l.Severity == "" && used >= 75:
		c = cYellow
	}
	st := lipgloss.NewStyle().Foreground(c)
	return st.Render(strings.Repeat("█", n)) + sDim.Render(strings.Repeat("░", usageBar-n)) + st.Render(fmt.Sprintf(" %3.0f%% used", used))
}

// resetText says when a window resets: "resets in 3h 18m (Thu 15:56)".
func resetText(now, at time.Time) string {
	switch {
	case at.IsZero():
		return ""
	case !at.After(now):
		return "reset"
	}
	return "resets in " + untilText(at.Sub(now)) + " (" + at.Local().Format("Mon 15:04") + ")"
}

// untilText is a duration in its two largest units: "4d 2h", "3h 18m", "7m".
func untilText(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dm", int(math.Ceil(d.Minutes())))
}
