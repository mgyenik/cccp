package ui

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"cccp/internal/core"
	"cccp/internal/fuzzy"
	"cccp/internal/workstream"
)

// mover picks the workstream a session moves to (M in the cockpit).
type mover struct {
	sess    core.Session
	input   textinput.Model
	all     []moveTarget // every workstream but the session's own
	choices []moveTarget
	cursor  int
	err     string
}

// moveTarget is one row: an existing workstream, or a new one to create.
type moveTarget struct {
	Name    string
	Dormant bool // restored first
	New     bool
}

func newMover(s core.Session, snap core.Snapshot) *mover {
	in := textinput.New()
	in.Prompt = "move to: "
	in.Placeholder = "a workstream (fuzzy), or a new name"
	m := &mover{sess: s, input: in}
	for _, w := range snap.Workstreams.Workstreams {
		if w.Name != s.TmuxSess {
			m.all = append(m.all, moveTarget{Name: w.Name, Dormant: w.Dormant})
		}
	}
	m.refresh()
	return m
}

// moveTargets ranks the workstreams for query: fuzzy matches, best first
// (running before dormant on a tie, then by name); a valid name matching no
// workstream exactly adds a "new workstream" row after them (Enter takes the
// best match; creating is always that explicit row).
func moveTargets(query string, all []moveTarget) []moveTarget {
	q := strings.TrimSpace(query)
	type scored struct {
		t     moveTarget
		score int
	}
	var hits []scored
	exact := false
	for _, t := range all {
		if s, ok := fuzzy.Match(q, t.Name); ok {
			hits = append(hits, scored{t, s})
		}
		exact = exact || t.Name == q
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.t.Dormant != b.t.Dormant {
			return !a.t.Dormant
		}
		return a.t.Name < b.t.Name
	})
	var out []moveTarget
	for _, h := range hits {
		out = append(out, h.t)
	}
	if q != "" && !exact && workstream.Validate(q) == nil {
		out = append(out, moveTarget{Name: q, New: true})
	}
	return out
}

func (m *mover) refresh() {
	m.choices = moveTargets(m.input.Value(), m.all)
	m.cursor = 0
}

// update handles a key; done with dest "" means cancelled.
func (m *mover) update(k tea.KeyPressMsg) (cmd tea.Cmd, done bool, dest string) {
	switch k.String() {
	case "esc":
		return nil, true, ""
	case "up", "ctrl+p":
		if m.cursor > 0 {
			m.cursor--
		}
		return nil, false, ""
	case "down", "ctrl+n":
		if m.cursor < len(m.choices)-1 {
			m.cursor++
		}
		return nil, false, ""
	case "enter":
		if m.cursor < len(m.choices) {
			return nil, true, m.choices[m.cursor].Name
		}
		if err := workstream.Validate(strings.TrimSpace(m.input.Value())); err != nil {
			m.err = err.Error()
		} else {
			m.err = "no workstream matches"
		}
		return nil, false, ""
	}
	before := m.input.Value()
	m.input, cmd = m.input.Update(k)
	if m.input.Value() != before {
		m.err = ""
		m.refresh()
	}
	return cmd, false, ""
}

func (m *mover) view(w, h int) string {
	from := "a new window"
	if m.sess.TmuxSess != "" {
		from = "workstream " + m.sess.TmuxSess
	}
	what := "its window moves, with everything in it"
	if !(m.sess.Alive && m.sess.Pane != "") {
		what = "it opens in a new window there"
	}
	lines := []string{
		sTitle.Render(ansi.Truncate(fmt.Sprintf("Move %q (now in %s) to another workstream; %s.", m.sess.DisplayTitle(), from, what), w, "…")),
		"",
	}
	if m.err != "" {
		lines = append(lines, sStatus.Render(m.err), "")
	}
	rows := h - len(lines)
	for i, t := range m.choices {
		if i >= rows {
			break
		}
		s := "  " + t.Name
		switch {
		case t.New:
			s = "+ new workstream " + t.Name
		case t.Dormant:
			s += sDim.Render("  (dormant: restored first)")
		}
		s = ansi.Truncate(s, w, "…")
		if i == m.cursor {
			s = sCursor.Render(pad(ansi.Strip(s), w))
		} else if t.New {
			s = lipgloss.NewStyle().Foreground(cGreen).Render(s)
		}
		lines = append(lines, s)
	}
	if len(m.choices) == 0 {
		lines = append(lines, sDim.Render("  no other workstream yet: type a name to make one"))
	}
	return lipgloss.NewStyle().Height(h).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

// movedMsg reports a move's outcome.
type movedMsg struct {
	title, dest string
	err         error
}
