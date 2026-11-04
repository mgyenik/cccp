package ui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"cccp/internal/core"
	"cccp/internal/dirpick"
	"cccp/internal/workstream"
)

// Dirs reads directories for the directory picker (dirs.FS).
type Dirs interface {
	Walk(root string) []string
	List(dir string) []string
	Exists(path string) bool
}

// creator is the two-step "new workstream" flow: a name, then a directory
// (docs/workstreams.md). It is used inside the cockpit (w) and on its own
// (`cccp start` when there is no workstream yet).
type creator struct {
	step    createStep
	name    textinput.Model
	dir     textinput.Model
	src     dirpick.Source
	walk    func() []string // nil: no walk
	walking bool
	choices []dirpick.Choice
	cursor  int
	offset  int
	// names are the workstreams that exist (running or dormant): typing
	// one of them opens it instead.
	names map[string]bool
	err   string
}

type createStep int

const (
	stepName createStep = iota
	stepDir
)

// Created is what the creator decided.
type Created struct {
	Open   string // an existing workstream to open; or:
	Name   string // a new workstream,
	Dir    string // in this directory,
	Create bool   // which does not exist yet and is to be made.
}

// walkedMsg delivers the walk under $HOME.
type walkedMsg struct{ dirs []string }

// choiceLimit bounds the picker's list.
const choiceLimit = 500

func newCreator(snap core.Snapshot, home string, d Dirs) *creator {
	n := textinput.New()
	n.Prompt = "workstream: "
	n.Placeholder = "name (existing ones open; no '.' or ':')"
	p := textinput.New()
	p.Prompt = "directory: "
	p.Placeholder = "fuzzy search, or a path (~/…, /…); Tab completes"
	c := &creator{name: n, dir: p, names: map[string]bool{}}
	c.src = dirpick.Source{Home: home, Known: knownDirs(snap),
		List: func(string) []string { return nil }, Exists: func(string) bool { return false }}
	if d != nil {
		c.src.List, c.src.Exists = d.List, d.Exists
		c.walk = func() []string { return d.Walk(home) }
	}
	for _, w := range snap.Workstreams.Workstreams {
		c.names[w.Name] = true
	}
	return c
}

// knownDirs are the directories cccp has seen in use: conversations' and
// workstream panes' working directories, with when.
func knownDirs(snap core.Snapshot) []dirpick.Known {
	var out []dirpick.Known
	for _, s := range snap.Sessions {
		if s.Cwd != "" {
			out = append(out, dirpick.Known{Path: s.Cwd, At: s.ActivitySince})
		}
	}
	for _, w := range snap.Workstreams.Workstreams {
		for _, win := range w.Windows {
			for _, p := range win.Panes {
				if p.Cwd != "" {
					out = append(out, dirpick.Known{Path: p.Cwd, At: w.LastUsed})
				}
			}
		}
	}
	return out
}

func (c *creator) focus() tea.Cmd { return c.name.Focus() }

// update handles a key. done: the flow finished; res is nil when it was
// cancelled.
func (c *creator) update(k tea.KeyPressMsg) (cmd tea.Cmd, done bool, res *Created) {
	key := k.String()
	switch c.step {
	case stepName:
		switch key {
		case "esc":
			return nil, true, nil
		case "enter":
			name := c.name.Value()
			if err := workstream.Validate(name); err != nil {
				c.err = err.Error()
				return nil, false, nil
			}
			if c.names[name] {
				return nil, true, &Created{Open: name}
			}
			c.err = ""
			c.step = stepDir
			c.name.Blur()
			c.refresh()
			var walk tea.Cmd
			if c.walk != nil && c.src.Walked == nil && !c.walking {
				c.walking = true
				w := c.walk
				walk = func() tea.Msg { return walkedMsg{dirs: w()} }
			}
			return tea.Batch(c.dir.Focus(), walk), false, nil
		}
		c.name, cmd = c.name.Update(k)
		c.err = ""
		return cmd, false, nil
	default:
		switch key {
		case "esc":
			c.step = stepName
			c.dir.Blur()
			return c.name.Focus(), false, nil
		case "up", "ctrl+p":
			if c.cursor > 0 {
				c.cursor--
			}
			return nil, false, nil
		case "down", "ctrl+n":
			if c.cursor < len(c.choices)-1 {
				c.cursor++
			}
			return nil, false, nil
		case "tab":
			if ch, ok := c.selected(); ok {
				c.dir.SetValue(dirpick.Display(ch.Path, c.src.Home) + "/")
				c.dir.CursorEnd()
				c.refresh()
			}
			return nil, false, nil
		case "enter":
			ch, ok := c.selected()
			if !ok {
				c.err = "no directory matches; type a path (~/… or /…) to create one"
				return nil, false, nil
			}
			return nil, true, &Created{Name: c.name.Value(), Dir: ch.Path, Create: ch.Create}
		}
		before := c.dir.Value()
		c.dir, cmd = c.dir.Update(k)
		if c.dir.Value() != before {
			c.err = ""
			c.refresh()
		}
		return cmd, false, nil
	}
}

// walked takes the walk's result.
func (c *creator) walked(dirs []string) {
	c.walking = false
	c.src.Walked = dirs
	if dirs == nil {
		c.src.Walked = []string{} // walked, nothing found: do not walk again
	}
	c.refresh()
}

func (c *creator) refresh() {
	c.choices = dirpick.Choices(c.dir.Value(), c.src, choiceLimit)
	c.cursor, c.offset = 0, 0
}

func (c *creator) selected() (dirpick.Choice, bool) {
	if c.cursor < len(c.choices) {
		return c.choices[c.cursor], true
	}
	return dirpick.Choice{}, false
}

// input is the line being typed.
func (c *creator) input() string {
	if c.step == stepName {
		return c.name.View()
	}
	return c.dir.View()
}

// view renders the flow's body: the directory choices once there.
func (c *creator) view(w, h int) string {
	var lines []string
	title := "New workstream: a tmux session. Type its name; an existing one opens."
	if c.step == stepDir {
		title = fmt.Sprintf("New workstream %q: pick its directory (it opens a fresh tmux session there).", c.name.Value())
	}
	lines = append(lines, sTitle.Render(ansi.Truncate(title, w, "…")), "")
	if c.err != "" {
		lines = append(lines, sStatus.Render(c.err), "")
	}
	if c.step == stepDir {
		rows := h - len(lines) - 1
		if rows < 1 {
			rows = 1
		}
		if c.cursor < c.offset {
			c.offset = c.cursor
		}
		if c.cursor >= c.offset+rows {
			c.offset = c.cursor - rows + 1
		}
		for i := c.offset; i < len(c.choices) && i < c.offset+rows; i++ {
			ch := c.choices[i]
			s := "  " + dirpick.Display(ch.Path, c.src.Home)
			if ch.Create {
				s = "+ create " + dirpick.Display(ch.Path, c.src.Home)
			}
			s = ansi.Truncate(s, w, "…")
			if i == c.cursor {
				s = sCursor.Render(pad(s, w))
			} else if ch.Create {
				s = lipgloss.NewStyle().Foreground(cGreen).Render(s)
			}
			lines = append(lines, s)
		}
		if len(c.choices) == 0 {
			lines = append(lines, sDim.Render("  no directory matches"))
		}
		if c.walking {
			lines = append(lines, sDim.Render("  (looking under ~ …)"))
		}
	}
	return lipgloss.NewStyle().Height(h).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

func (c *creator) hint() string {
	if c.step == stepName {
		return "enter next · esc cancel"
	}
	return "enter open · ↑↓ move · tab complete · esc back"
}

// createProgram runs the creator on its own.
type createProgram struct {
	c             *creator
	res           *Created
	width, height int
}

func (p *createProgram) Init() tea.Cmd { return p.c.focus() }

func (p *createProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width, p.height = msg.Width, msg.Height
		p.c.name.SetWidth(msg.Width - 14)
		p.c.dir.SetWidth(msg.Width - 13)
		return p, nil
	case walkedMsg:
		p.c.walked(msg.dirs)
		return p, nil
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return p, tea.Quit
		}
		cmd, done, res := p.c.update(msg)
		if done {
			p.res = res
			return p, tea.Quit
		}
		return p, cmd
	}
	var cmd tea.Cmd
	if p.c.step == stepName {
		p.c.name, cmd = p.c.name.Update(msg)
	} else {
		p.c.dir, cmd = p.c.dir.Update(msg)
	}
	return p, cmd
}

func (p *createProgram) View() tea.View {
	w, h := p.width, p.height
	if w == 0 {
		w, h = 80, 24
	}
	body := p.c.view(w, h-1)
	v := tea.NewView(lipgloss.JoinVertical(lipgloss.Left, body,
		ansi.Truncate(p.c.input()+"  "+sKeyHint.Render(p.c.hint()), w, "…")))
	v.AltScreen = true
	return v
}

// RunCreate asks on the terminal for a workstream: an existing one to open,
// or a new one's name and directory. nil: cancelled.
func RunCreate(snap core.Snapshot, home string, d Dirs) (*Created, error) {
	p := &createProgram{c: newCreator(snap, home, d)}
	if _, err := tea.NewProgram(p).Run(); err != nil {
		return nil, err
	}
	return p.res, nil
}
