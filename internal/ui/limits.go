package ui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"cccp/internal/core"
	"cccp/internal/jail"
	"cccp/internal/present"
)

// JailStats reads a slice's live numbers (cgroups.FS.Stats).
type JailStats func(slice string) jail.Stats

// jailRead is how often the cockpit reads the slices' numbers, while open.
const jailRead = 2 * time.Second

// jailSample is a slice's numbers as last read, with the CPU use (cores)
// since the read before.
type jailSample struct {
	stats    jail.Stats
	at       time.Time
	cpu      float64
	cpuKnown bool
}

// nearLimit: the slice uses at least jail.WarnAt of its memory limit.
func (s jailSample) nearLimit() bool {
	return s.stats.Max > 0 && float64(s.stats.Memory) >= jail.WarnAt*float64(s.stats.Max)
}

// jailStatsMsg delivers a read of every slice.
type jailStatsMsg struct {
	at    time.Time
	stats map[string]jail.Stats
}

// readJail reads the running workstreams' slices (and A's).
func readJail(read JailStats, j core.Jail, at time.Time) tea.Cmd {
	slices := []string{jail.Root}
	for _, w := range j.Workstreams {
		slices = append(slices, w.Slice)
	}
	return func() tea.Msg {
		out := map[string]jail.Stats{}
		for _, s := range slices {
			out[s] = read(s)
		}
		return jailStatsMsg{at: at, stats: out}
	}
}

// sample folds a new read into the previous one.
func sample(prev jailSample, st jail.Stats, at time.Time) jailSample {
	s := jailSample{stats: st, at: at}
	if !prev.at.IsZero() && at.After(prev.at) && st.CPUUsecs >= prev.stats.CPUUsecs {
		s.cpu = float64(st.CPUUsecs-prev.stats.CPUUsecs) / 1e6 / at.Sub(prev.at).Seconds()
		s.cpuKnown = true
	}
	return s
}

// jailLines are a workstream's jail in the preview: its state (preset,
// boost, near its limit, kills), then its numbers.
func jailLines(w core.JailWorkstream, s jailSample, now time.Time) []string {
	st := s.stats
	state := []string{"jail " + w.Name, w.Preset}
	if w.Boosted {
		left := "until undone"
		if !w.Until.IsZero() {
			left = present.Age(w.Until.Sub(now)) + " left"
		}
		state[1] = "⇧ " + w.Preset + ", " + left
	}
	if s.nearLimit() {
		state = append(state, "near its memory limit")
	}
	if st.OOMKills > 0 {
		state = append(state, fmt.Sprintf("%d killed at the limit", st.OOMKills))
	}
	nums := fmt.Sprintf("memory %s of %s", present.Bytes(st.Memory), present.Bytes(st.Max))
	if s.cpuKnown {
		nums += fmt.Sprintf(" · cpu %.1f cores", s.cpu)
	}
	return []string{strings.Join(state, " · "), "  " + nums}
}

// limitChoice is one row of the limits picker.
type limitChoice struct {
	Preset string
	For    time.Duration // 0: until undone (a boost) or none (normal)
}

var limitChoices = []limitChoice{
	{jail.PresetNormal, 0},
	{jail.PresetBoost, time.Hour}, {jail.PresetBoost, 2 * time.Hour}, {jail.PresetBoost, 8 * time.Hour}, {jail.PresetBoost, 0},
	{jail.PresetMax, time.Hour}, {jail.PresetMax, 2 * time.Hour}, {jail.PresetMax, 0},
}

func (c limitChoice) label(ws core.JailWorkstream) string {
	switch {
	case c.Preset == jail.PresetNormal && ws.NormalPreset != "" && ws.NormalPreset != jail.PresetNormal:
		return "normal (" + ws.NormalPreset + ")" // its own configured limits
	case c.Preset == jail.PresetNormal:
		return "normal"
	case c.For == 0:
		return c.Preset + " until undone"
	}
	return c.Preset + " for " + present.Age(c.For)
}

// limiter picks a workstream's preset (L in the cockpit).
type limiter struct {
	ws     core.JailWorkstream
	j      core.Jail
	memory int64 // in use now
	cursor int
}

func newLimiter(ws core.JailWorkstream, j core.Jail, memory int64) *limiter {
	return &limiter{ws: ws, j: j, memory: memory}
}

// update handles a key; done with a nil choice means cancelled.
func (l *limiter) update(k tea.KeyPressMsg) (done bool, c *limitChoice) {
	switch k.String() {
	case "esc", "q":
		return true, nil
	case "up", "k":
		if l.cursor > 0 {
			l.cursor--
		}
	case "down", "j":
		if l.cursor < len(limitChoices)-1 {
			l.cursor++
		}
	case "enter":
		c := limitChoices[l.cursor]
		return true, &c
	}
	return false, nil
}

// limits are what choosing c gives the workstream: normal is its own
// configured limits (what ending a boost returns it to), a boost the preset.
func (l *limiter) limits(c limitChoice) jail.Limits {
	if c.Preset == jail.PresetNormal && l.ws.NormalPreset != "" {
		return l.ws.Normal
	}
	return l.j.Presets[c.Preset]
}

// warning is what limits lim would do to a workstream using memory bytes
// now: a limit below it makes the kernel kill a process in it at once.
func warning(lim jail.Limits, total, memory int64) string {
	limit := jail.LimitBytes(lim.MemoryMax, total)
	if limit > 0 && memory > limit {
		return "below current use: the kernel would kill a process at once"
	}
	return ""
}

func (l *limiter) view(w, h int) string {
	lines := []string{
		sTitle.Render(ansi.Truncate(fmt.Sprintf("Limits of workstream %q: using %s now (preset %s).", l.ws.Name, present.Bytes(l.memory), l.ws.Preset), w, "…")),
		sDim.Render("boost: more memory and CPU than other workstreams · max: also above the desktop"),
		"",
	}
	for i, c := range limitChoices {
		p := l.limits(c)
		s := fmt.Sprintf("  %-20s memory %-9s cpu %d", c.label(l.ws), present.Bytes(jail.LimitBytes(p.MemoryMax, l.j.MemTotal)), p.CPUWeight)
		if warn := warning(p, l.j.MemTotal, l.memory); warn != "" {
			s += "  ! " + warn
		}
		s = ansi.Truncate(s, w, "…")
		if i == l.cursor {
			s = sCursor.Render(pad(s, w))
		}
		lines = append(lines, s)
	}
	return lipgloss.NewStyle().Height(h).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

// limitsMsg reports a limits change.
type limitsMsg struct {
	text string
	err  error
}

// jailSampleOf is a workstream's numbers: as last read by the cockpit, else
// the snapshot's.
func (m *Model) jailSampleOf(w core.JailWorkstream) jailSample {
	if s, ok := m.jail[w.Slice]; ok {
		return s
	}
	return jailSample{stats: w.Stats}
}
