// Package ui is the cockpit: a Bubble Tea TUI over the engine snapshot.
package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"cccp/internal/catalog"
	"cccp/internal/core"
	"cccp/internal/fuzzy"
	"cccp/internal/model"
	"cccp/internal/present"
)

// Source feeds snapshots to the cockpit.
type Source interface {
	// Start begins delivering snapshots through send.
	Start(send func(core.Snapshot))
	// Refresh asks for a full rescan.
	Refresh()
	// Daemon reports whether a daemon serves the snapshots.
	Daemon() bool
	Close()
}

// Actions are side effects the cockpit asks for.
type Actions struct {
	Jump  func(s core.Session) error
	Alias func(key, value string) error
	Seen  func(key string) error
	// Summarize asks for a summary of a closed conversation (may be nil).
	Summarize func(key string) error
	// OpenWorkstream switches to a workstream, restoring it first if it is
	// dormant; NewWorkstream creates one in dir (making dir if create) and
	// switches to it. Either may be nil (no workstream support).
	OpenWorkstream func(name string) error
	NewWorkstream  func(name, dir string, create bool) error
	// MoveSession moves a session into the workstream dest (made, or
	// restored, if need be); the cockpit stays open. May be nil.
	MoveSession func(s core.Session, dest string) error
}

// Searcher searches the conversation text (catalog.Index). The cockpit only
// reads it: the daemon, or the in-process source, keeps it up to date.
type Searcher interface {
	Run(q catalog.Query, limit int) ([]catalog.Hit, error)
}

// PaneCapturer captures a tmux pane's contents.
type PaneCapturer interface {
	CapturePane(pane string, ansi bool, lines int) (string, error)
}

// Options configure the cockpit.
type Options struct {
	Source  Source
	Actions Actions
	Panes   PaneCapturer // nil: no live preview
	Search  Searcher     // nil: search disabled
	Dirs    Dirs         // the directory picker's filesystem (nil: known directories only)
	Home    string
	// Workstream is the tmux session the cockpit's client is on ("" if
	// unknown), shown in the header.
	Workstream string
}

type mode int

const (
	modeList mode = iota
	modeFilter
	modeAlias
	modeCreate // the new-workstream flow (creator)
	modeMove   // moving a session to another workstream (mover)
)

type snapMsg core.Snapshot
type tickMsg time.Time
type previewMsg struct {
	key  string
	text string
	err  error
}
type searchMsg struct {
	query string
	hits  []catalog.Hit
	err   error
}

// contentTick fires after a pause in typing: time to search the text.
type contentTick struct{ query string }

// contentDelay is the pause in typing after which conversation text is
// searched.
const contentDelay = 150 * time.Millisecond

type doneMsg struct{ err error }

// Model is the Bubble Tea model.
type Model struct {
	opt Options

	snap    core.Snapshot
	hasSnap bool
	now     time.Time

	width, height int

	mode       mode
	cursor     int // index into visible()
	offset     int
	hideClosed bool
	preview    previewMode

	filter textinput.Model
	alias  textinput.Model

	// content holds the conversation-text matches for contentQuery.
	content      map[string]catalog.Hit
	contentQuery string
	// summarized: closed conversations already asked to be summarised.
	summarized map[string]bool

	previewKey  string
	previewText string

	creator *creator // the new-workstream flow, in modeCreate
	mover   *mover   // the move flow, in modeMove

	status   string
	statusAt time.Time
	quitting bool

	selectKey string // keep the selection on the same session across updates
}

type previewMode int

const (
	previewAuto previewMode = iota
	previewPane
	previewConversation
	previewOff
)

// New creates the cockpit model.
func New(opt Options) *Model {
	f := textinput.New()
	f.Prompt = "/ "
	f.Placeholder = "search everything: titles, projects, prompts, conversation text…"
	a := textinput.New()
	a.Prompt = "alias: "
	return &Model{opt: opt, filter: f, alias: a, now: time.Now(), summarized: map[string]bool{}}
}

// Run starts the cockpit and blocks until it exits.
func Run(opt Options) error {
	m := New(opt)
	p := tea.NewProgram(m)
	opt.Source.Start(func(s core.Snapshot) { p.Send(snapMsg(s)) })
	defer opt.Source.Close()
	_, err := p.Run()
	return err
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd { return tick() }

// visible returns the sessions shown in the list, in order.
func (m *Model) visible() []core.Session {
	q := strings.TrimSpace(m.filter.Value())
	var content map[string]catalog.Hit
	if q != "" && m.contentQuery == q {
		content = m.content
	}
	return rankSessions(m.snap.Sessions, q, content, m.hideClosed)
}

// rankSessions is the list's search. Without a query: every session in
// snapshot order (grouped), closed ones unless hidden. With a query: one
// flat list, fuzzy matches on the session's facts ranked by score, then
// sessions found only in their conversation text, in the text index's
// order (newest first). Closed conversations are always searched.
func rankSessions(ss []core.Session, q string, content map[string]catalog.Hit, hideClosed bool) []core.Session {
	if q == "" {
		var out []core.Session
		for _, s := range ss {
			if s.Group == core.GroupClosed && hideClosed {
				continue
			}
			out = append(out, s)
		}
		return out
	}
	type scored struct {
		s     core.Session
		score int
		pos   int
	}
	var hits []scored
	matched := map[string]bool{}
	for i, s := range ss {
		if score, ok := fuzzy.MatchFields(q, fields(s)...); ok {
			hits = append(hits, scored{s, score, i})
			matched[s.Key] = true
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].pos < hits[j].pos
	})
	out := make([]core.Session, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.s)
	}
	var text []scored
	for i, s := range ss {
		if _, ok := content[s.Key]; ok && !matched[s.Key] {
			text = append(text, scored{s, 0, i})
		}
	}
	sort.SliceStable(text, func(i, j int) bool {
		return content[text[i].s.Key].At.After(content[text[j].s.Key].At)
	})
	for _, t := range text {
		out = append(out, t.s)
	}
	return out
}

// fields is what the search matches a session against: names and paths
// fuzzily, prompts and the summary as prose.
func fields(s core.Session) []fuzzy.Field {
	short := func(t string) fuzzy.Field { return fuzzy.Field{Text: t} }
	prose := func(t string) fuzzy.Field { return fuzzy.Field{Text: t, Prose: true} }
	return []fuzzy.Field{
		short(s.DisplayTitle()), short(s.Title), short(s.Profile), short(string(s.Agent)),
		short(s.Group.String()), short(s.Cwd), short(s.Location), short(s.Branch),
		prose(s.FirstPrompt), prose(s.LastPrompt), prose(s.Summary),
	}
}

func (m *Model) selected() (core.Session, bool) {
	v := m.visible()
	if m.cursor < 0 || m.cursor >= len(v) {
		return core.Session{}, false
	}
	return v[m.cursor], true
}

func (m *Model) setStatus(s string) {
	m.status, m.statusAt = s, time.Now()
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.filter.SetWidth(msg.Width - 4)
		m.alias.SetWidth(msg.Width - 12)
		return m, m.loadPreview()

	case snapMsg:
		m.snap, m.hasSnap = core.Snapshot(msg), true
		m.restoreSelection()
		return m, m.loadPreview()

	case tickMsg:
		m.now = time.Time(msg)
		return m, tea.Batch(tick(), m.loadPreview())

	case previewMsg:
		if msg.key == m.previewKey {
			if msg.err != nil {
				m.previewText = "(pane unavailable: " + msg.err.Error() + ")"
			} else {
				m.previewText = msg.text
			}
		}
		return m, nil

	case contentTick:
		if msg.query != strings.TrimSpace(m.filter.Value()) || msg.query == "" || m.opt.Search == nil {
			return m, nil // typing went on, or nothing to search
		}
		return m, runSearch(m.opt.Search, msg.query)

	case searchMsg:
		if msg.query != strings.TrimSpace(m.filter.Value()) {
			return m, nil // a stale result
		}
		if msg.err != nil {
			m.setStatus("text search: " + msg.err.Error())
			return m, nil
		}
		m.content, m.contentQuery = map[string]catalog.Hit{}, msg.query
		for _, h := range msg.hits {
			m.content[h.Doc.Key()] = h
		}
		m.restoreSelection()
		return m, m.loadPreview()

	case doneMsg:
		if msg.err != nil {
			m.setStatus("error: " + msg.err.Error())
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit

	case walkedMsg:
		if m.creator != nil {
			m.creator.walked(msg.dirs)
		}
		return m, nil

	case movedMsg:
		if msg.err != nil {
			m.setStatus("move: " + msg.err.Error())
		} else {
			m.setStatus(fmt.Sprintf("moved %q to %s", msg.title, msg.dest))
			m.opt.Source.Refresh()
		}
		return m, nil

	case tea.KeyPressMsg:
		return m.key(msg)
	}

	// Forward everything else (cursor blink etc.) to the focused input.
	var cmd tea.Cmd
	switch m.mode {
	case modeFilter:
		m.filter, cmd = m.filter.Update(msg)
	case modeAlias:
		m.alias, cmd = m.alias.Update(msg)
	case modeMove:
		m.mover.input, cmd = m.mover.input.Update(msg)
	case modeCreate:
		if m.creator.step == stepName {
			m.creator.name, cmd = m.creator.name.Update(msg)
		} else {
			m.creator.dir, cmd = m.creator.dir.Update(msg)
		}
	}
	return m, cmd
}

func (m *Model) restoreSelection() {
	v := m.visible()
	if m.selectKey != "" {
		for i, s := range v {
			if s.Key == m.selectKey {
				m.cursor = i
				return
			}
		}
	}
	if m.cursor >= len(v) {
		m.cursor = len(v) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.rememberSelection()
}

func (m *Model) rememberSelection() {
	if s, ok := m.selected(); ok {
		m.selectKey = s.Key
	}
}

func (m *Model) move(d int) {
	n := len(m.visible())
	if n == 0 {
		return
	}
	m.cursor = (m.cursor + d + n) % n
	m.rememberSelection()
}

func (m *Model) key(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.mode {
	case modeMove:
		cmd, done, dest := m.mover.update(k)
		if !done {
			return m, cmd
		}
		s := m.mover.sess
		m.mode, m.mover = modeList, nil
		if dest == "" {
			return m, cmd
		}
		move := m.opt.Actions.MoveSession
		return m, tea.Batch(cmd, func() tea.Msg {
			return movedMsg{title: s.DisplayTitle(), dest: dest, err: move(s, dest)}
		})
	case modeCreate:
		cmd, done, res := m.creator.update(k)
		if !done {
			return m, cmd
		}
		m.mode, m.creator = modeList, nil
		if res == nil {
			return m, cmd
		}
		return m, tea.Batch(cmd, m.workstreamAction(*res))
	case modeFilter:
		switch key {
		case "esc":
			m.filter.SetValue("")
			m.filter.Blur()
			m.mode = modeList
			m.content, m.contentQuery = nil, ""
			m.restoreSelection()
			return m, m.loadPreview()
		case "enter":
			m.filter.Blur()
			m.mode = modeList
			return m, m.jumpSelected()
		case "up", "ctrl+p":
			m.move(-1)
			return m, m.loadPreview()
		case "down", "ctrl+n", "tab":
			m.move(1)
			return m, m.loadPreview()
		}
		var cmd tea.Cmd
		before := m.filter.Value()
		m.filter, cmd = m.filter.Update(k)
		if m.filter.Value() == before {
			return m, cmd
		}
		m.cursor = 0
		m.rememberSelection()
		q := strings.TrimSpace(m.filter.Value())
		return m, tea.Batch(cmd, m.loadPreview(),
			tea.Tick(contentDelay, func(time.Time) tea.Msg { return contentTick{query: q} }))

	case modeAlias:
		switch key {
		case "esc":
			m.alias.Blur()
			m.mode = modeList
			return m, nil
		case "enter":
			s, ok := m.selected()
			m.alias.Blur()
			m.mode = modeList
			if ok && m.opt.Actions.Alias != nil {
				if err := m.opt.Actions.Alias(s.Key, m.alias.Value()); err != nil {
					m.setStatus("alias: " + err.Error())
				} else {
					m.setStatus("alias saved")
				}
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.alias, cmd = m.alias.Update(k)
		return m, cmd
	}

	// List mode.
	switch key {
	case "q", "esc":
		return m, tea.Quit
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "home", "g":
		m.cursor = 0
		m.rememberSelection()
	case "end", "G":
		m.cursor = len(m.visible()) - 1
		m.rememberSelection()
	case "tab":
		m.nextAttention(1)
	case "shift+tab":
		m.nextAttention(-1)
	case "enter":
		return m, m.jumpSelected()
	case "/":
		m.mode = modeFilter
		return m, m.filter.Focus()
	case "e":
		m.hideClosed = !m.hideClosed
		m.restoreSelection()
	case "p":
		// Cycle from what is shown now: pane → conversation → off → pane.
		cur := previewOff
		if sel, ok := m.selected(); ok {
			cur = m.effectivePreview(sel)
		}
		switch cur {
		case previewPane:
			m.preview = previewConversation
		case previewConversation:
			m.preview = previewOff
		default:
			m.preview = previewPane
		}
	case "r":
		m.opt.Source.Refresh()
		m.setStatus("rescanning…")
	case "m":
		if s, ok := m.selected(); ok && m.opt.Actions.Seen != nil {
			if err := m.opt.Actions.Seen(s.Key); err != nil {
				m.setStatus("mark seen: " + err.Error())
			} else {
				m.setStatus("marked seen")
			}
		}
	case "a":
		if s, ok := m.selected(); ok {
			m.mode = modeAlias
			m.alias.SetValue(s.Alias)
			m.alias.CursorEnd()
			return m, m.alias.Focus()
		}
	case "M":
		s, ok := m.selected()
		if !ok {
			break
		}
		if m.opt.Actions.MoveSession == nil {
			m.setStatus("moving sessions is not available here")
			break
		}
		m.mover = newMover(s, m.snap)
		m.mode = modeMove
		return m, m.mover.input.Focus()
	case "w":
		if m.opt.Actions.NewWorkstream == nil {
			m.setStatus("workstreams are not available here")
			break
		}
		m.creator = newCreator(m.snap, m.opt.Home, m.opt.Dirs)
		m.mode = modeCreate
		return m, m.creator.focus()
	}
	return m, m.loadPreview()
}

// workstreamAction carries out the creator's decision; like a jump, the
// cockpit closes when it succeeds.
func (m *Model) workstreamAction(res Created) tea.Cmd {
	if res.Open != "" {
		open := m.opt.Actions.OpenWorkstream
		if open == nil {
			return nil
		}
		return func() tea.Msg { return doneMsg{err: open(res.Open)} }
	}
	newWS := m.opt.Actions.NewWorkstream
	return func() tea.Msg { return doneMsg{err: newWS(res.Name, res.Dir, res.Create)} }
}

func (m *Model) nextAttention(dir int) {
	v := m.visible()
	n := len(v)
	for i := 1; i <= n; i++ {
		j := (m.cursor + dir*i + n*2) % n
		if v[j].NeedsAttention() {
			m.cursor = j
			m.rememberSelection()
			return
		}
	}
	m.setStatus("nothing needs you")
}

func (m *Model) jumpSelected() tea.Cmd {
	s, ok := m.selected()
	if !ok || m.opt.Actions.Jump == nil {
		return nil
	}
	jump := m.opt.Actions.Jump
	return func() tea.Msg { return doneMsg{err: jump(s)} }
}

// runSearch searches the conversation text. The catalog is kept up to date
// by the daemon (or was brought up to date when the cockpit started), so it
// is only read here.
func runSearch(ix Searcher, q string) tea.Cmd {
	return func() tea.Msg {
		hits, err := ix.Run(catalog.ParseQuery(q), 500)
		return searchMsg{query: q, hits: hits, err: err}
	}
}

// effectivePreview resolves previewAuto for a session.
func (m *Model) effectivePreview(s core.Session) previewMode {
	if m.preview != previewAuto {
		return m.preview
	}
	if s.Pane != "" && s.Alive {
		return previewPane
	}
	return previewConversation
}

// loadPreview captures the selected pane, when the preview shows a pane,
// and asks for a summary of a selected closed conversation that has none.
func (m *Model) loadPreview() tea.Cmd {
	s, ok := m.selected()
	if !ok {
		m.previewKey, m.previewText = "", ""
		return nil
	}
	if s.Key != m.previewKey {
		m.previewKey, m.previewText = s.Key, ""
	}
	var ask tea.Cmd
	if s.Group == core.GroupClosed && s.Summary == "" && !m.summarized[s.Key] && m.opt.Actions.Summarize != nil {
		m.summarized[s.Key] = true
		summarize, key := m.opt.Actions.Summarize, s.Key
		ask = func() tea.Msg { _ = summarize(key); return nil }
	}
	if ask != nil {
		return tea.Batch(ask, m.capture(s))
	}
	return m.capture(s)
}

// capture returns the command capturing s's pane, if the preview shows it.
func (m *Model) capture(s core.Session) tea.Cmd {
	if m.effectivePreview(s) != previewPane || s.Pane == "" || m.opt.Panes == nil {
		return nil
	}
	tm, key, pane := m.opt.Panes, s.Key, s.Pane
	return func() tea.Msg {
		out, err := tm.CapturePane(pane, true, 0)
		return previewMsg{key: key, text: out, err: err}
	}
}

// ---- view -------------------------------------------------------------------

var (
	cYellow = lipgloss.Color("3")
	cGreen  = lipgloss.Color("2")
	cCyan   = lipgloss.Color("6")
	cBlue   = lipgloss.Color("4")
	cRed    = lipgloss.Color("1")
	cGray   = lipgloss.Color("8")

	sTitle   = lipgloss.NewStyle().Bold(true)
	sDim     = lipgloss.NewStyle().Foreground(cGray)
	sHeader  = lipgloss.NewStyle().Bold(true).Foreground(cGray)
	sCursor  = lipgloss.NewStyle().Reverse(true)
	sBorder  = lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(cGray).PaddingLeft(1)
	sStatus  = lipgloss.NewStyle().Foreground(cYellow)
	sKeyHint = lipgloss.NewStyle().Foreground(cGray)
)

func groupStyle(g core.Group) lipgloss.Style {
	switch g {
	case core.GroupNeedsYou:
		return lipgloss.NewStyle().Foreground(cYellow).Bold(true)
	case core.GroupYourTurn:
		return lipgloss.NewStyle().Foreground(cGreen).Bold(true)
	case core.GroupWorking:
		return lipgloss.NewStyle().Foreground(cCyan)
	case core.GroupCold:
		return lipgloss.NewStyle().Foreground(cBlue)
	case core.GroupClosed, core.GroupUnknown:
		return lipgloss.NewStyle().Foreground(cGray)
	}
	return lipgloss.NewStyle()
}

// View implements tea.Model.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "cccp"
	return v
}

func (m *Model) render() string {
	if m.quitting {
		return ""
	}
	if m.width == 0 || m.height == 0 {
		return "cccp: starting…"
	}
	if !m.hasSnap {
		return "cccp: loading sessions…"
	}
	header := m.renderHeader()
	footer := m.renderFooter()
	bodyH := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
	if bodyH < 3 {
		bodyH = 3
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, m.renderBody(bodyH), footer)
}

func (m *Model) renderHeader() string {
	c := m.snap.Counts()
	var parts []string
	for _, g := range core.Groups {
		if g == core.GroupClosed {
			continue
		}
		if n := c[g]; n > 0 {
			parts = append(parts, groupStyle(g).Render(fmt.Sprintf("%s %d %s", present.GroupGlyph(g), n, strings.ToLower(g.String()))))
		}
	}
	src := "in-process (daemon not running)"
	if m.opt.Source.Daemon() {
		src = "daemon"
	}
	if m.snap.OnBattery {
		src += " · battery"
	}
	left := sTitle.Render("cccp")
	if m.opt.Workstream != "" {
		left += " " + lipgloss.NewStyle().Foreground(cCyan).Render("["+m.opt.Workstream+"]")
	}
	left += "  " + strings.Join(parts, "  ")
	right := sDim.Render(src)
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	line := left + strings.Repeat(" ", gap) + right
	for _, p := range m.snap.Problems {
		line += "\n" + lipgloss.NewStyle().Foreground(cRed).Render("! "+p)
	}
	return line
}

func (m *Model) renderFooter() string {
	var line string
	switch m.mode {
	case modeMove:
		line = m.mover.input.View() + "  " + sKeyHint.Render("enter move · ↑↓ choose · esc cancel")
	case modeCreate:
		line = m.creator.input() + "  " + sKeyHint.Render(m.creator.hint())
	case modeFilter:
		line = m.filter.View() + "  " + sKeyHint.Render("enter open · ↑↓ move · esc clear")
	case modeAlias:
		line = m.alias.View()
	default:
		closed := "e hide closed"
		if m.hideClosed {
			closed = "e show closed"
		}
		hint := "enter open/resume · tab next needing you · / search  · w workstream · M move · a alias · m seen · p preview · " + closed + " · r rescan · q quit"
		if f := m.filter.Value(); f != "" {
			hint = "search: " + f + " · " + hint
		}
		line = sKeyHint.Render(ansi.Truncate(hint, m.width, "…"))
	}
	if m.status != "" && time.Since(m.statusAt) < 4*time.Second {
		line = sStatus.Render(m.status) + "  " + line
	}
	return ansi.Truncate(line, m.width, "…")
}

func (m *Model) renderBody(h int) string {
	if m.mode == modeCreate {
		return m.creator.view(m.width, h)
	}
	if m.mode == modeMove {
		return m.mover.view(m.width, h)
	}

	sel, hasSel := m.selected()
	pm := previewOff
	if hasSel {
		pm = m.effectivePreview(sel)
	}
	if pm == previewOff || m.width < 60 {
		return m.renderList(m.width, h)
	}
	if m.width >= 150 {
		lw := m.width * 55 / 100
		list := m.renderList(lw, h)
		prev := sBorder.Width(m.width - lw).Height(h).MaxHeight(h).Render(m.renderPreview(sel, pm, m.width-lw-2, h))
		return lipgloss.JoinHorizontal(lipgloss.Top, list, prev)
	}
	lh := h * 55 / 100
	if lh < 5 {
		return m.renderList(m.width, h)
	}
	list := m.renderList(m.width, lh)
	rule := sDim.Render(strings.Repeat("─", m.width))
	prev := m.renderPreview(sel, pm, m.width, h-lh-1)
	return lipgloss.JoinVertical(lipgloss.Left, list, rule, prev)
}

// renderList draws the grouped session list, scrolled to keep the cursor in
// view.
func (m *Model) renderList(w, h int) string {
	vis := m.visible()
	type line struct {
		text string
		row  int // session index, -1 for headers
	}
	var lines []line
	last := core.Group(-1)
	searching := strings.TrimSpace(m.filter.Value()) != ""
	wsW := workstreamWidth(vis)
	for i, s := range vis {
		if s.Group != last && !searching { // search results are one ranked list
			lines = append(lines, line{groupStyle(s.Group).Render(s.Group.String()), -1})
			last = s.Group
		}
		lines = append(lines, line{m.renderRow(s, w, wsW, i == m.cursor), i})
	}
	if len(vis) == 0 {
		msg := "no sessions"
		if searching {
			msg = "nothing matches"
		}
		lines = append(lines, line{sDim.Render(msg), -1})
	}
	// Scroll: find the cursor's line.
	cur := 0
	for i, l := range lines {
		if l.row == m.cursor {
			cur = i
		}
	}
	if cur < m.offset {
		m.offset = cur
		if m.offset > 0 && lines[m.offset-1].row == -1 {
			m.offset-- // keep the group header with its first row
		}
	}
	if cur >= m.offset+h {
		m.offset = cur - h + 1
	}
	if m.offset > len(lines)-h {
		m.offset = len(lines) - h
	}
	if m.offset < 0 {
		m.offset = 0
	}
	end := m.offset + h
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	for i := m.offset; i < end; i++ {
		b.WriteString(ansi.Truncate(lines[i].text, w, "…"))
		if i < end-1 {
			b.WriteByte('\n')
		}
	}
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(b.String())
}

func pad(s string, n int) string {
	w := ansi.StringWidth(s)
	if w > n {
		return ansi.Truncate(s, n, "…")
	}
	return s + strings.Repeat(" ", n-w)
}

// maxWorkstreamWidth bounds the workstream column.
const maxWorkstreamWidth = 16

// workstreamOf is a row's workstream column: its tmux session ("-" for a
// closed conversation, "background" for a background session).
func workstreamOf(s core.Session) string {
	switch {
	case s.TmuxSess != "":
		return s.TmuxSess
	case s.Location != "":
		return s.Location
	}
	return "-"
}

// workstreamWidth sizes the workstream column to the longest name shown.
func workstreamWidth(ss []core.Session) int {
	w := 1
	for _, s := range ss {
		w = max(w, ansi.StringWidth(workstreamOf(s)))
	}
	return min(w, maxWorkstreamWidth)
}

// renderRow is one session: state, harness, workstream, title, detail, and
// times. The profile and the exact tmux location are in the preview.
func (m *Model) renderRow(s core.Session, w, wsW int, cursor bool) string {
	g := present.Glyph(s)
	if s.Inferred {
		g += "~"
	} else {
		g += " "
	}
	turns := ""
	if s.Group == core.GroupClosed {
		turns = present.Turns(s.Turns)
	}
	meta := strings.Join(nonEmpty(
		present.Since(m.now, s.ActivitySince),
		present.CacheText(s, m.now),
		present.Tokens(s.ContextTokens),
		turns,
	), "  ")
	titleW := 34
	if w < 90 {
		titleW = 24
	}
	ws := ansi.Truncate(workstreamOf(s), wsW, "…")
	left := fmt.Sprintf(" %s %s %s %s ", g, present.AgentBadge(s.Agent), pad(ws, wsW), pad(s.DisplayTitle(), titleW))
	detailW := w - ansi.StringWidth(left) - ansi.StringWidth(meta) - 2
	detail := ""
	if detailW > 8 {
		d := present.Detail(s)
		if h, ok := m.content[s.Key]; ok && m.contentQuery == strings.TrimSpace(m.filter.Value()) {
			// Found in the conversation text: show the match, the column is narrow.
			d = "“" + present.Around(h.Snippet, catalog.ParseQuery(m.contentQuery).Terms, 10) + "”"
		} else if s.Group == core.GroupClosed && s.Summary != "" {
			d = s.Summary
		}
		detail = pad(present.OneLine(d), detailW)
	} else {
		detail = strings.Repeat(" ", max(0, detailW))
	}
	row := left + sDim.Render(detail) + "  " + meta
	if cursor {
		return sCursor.Render(ansi.Strip(row))
	}
	return glyphStyled(s, left) + sDim.Render(detail) + "  " + sDim.Render(meta)
}

// glyphStyled colours the glyph at the start of a row.
func glyphStyled(s core.Session, left string) string {
	r := []rune(left)
	if len(r) < 2 {
		return left
	}
	return string(r[:1]) + groupStyle(s.Group).Render(string(r[1:2])) + string(r[2:])
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func (m *Model) renderPreview(s core.Session, pm previewMode, w, h int) string {
	if w < 10 || h < 2 {
		return ""
	}
	var b strings.Builder
	head := sTitle.Render(ansi.Truncate(s.DisplayTitle(), w, "…"))
	info := []string{
		fmt.Sprintf("%s · %s · %s", s.Agent, s.Tracking, strings.ReplaceAll(string(s.Activity), "_", " ")),
	}
	if s.Profile != "" {
		info[0] = s.Profile + " profile · " + info[0]
	}
	if s.Inferred {
		info[0] += " (inferred)"
	}
	if s.Location != "" {
		info = append(info, "tmux "+s.Location)
	}
	if s.Cwd != "" {
		info = append(info, strings.Join(nonEmpty(s.Cwd, s.Branch), " · "))
	}
	if s.Group == core.GroupClosed {
		info = append(info, strings.Join(nonEmpty("closed "+present.Since(m.now, s.ActivitySince)+" ago",
			present.Turns(s.Turns), "enter resumes it"), " · "))
	}
	if s.Model != "" || s.ContextTokens > 0 {
		ctx := ""
		if s.ContextTokens > 0 {
			ctx = present.Tokens(s.ContextTokens) + " context"
		}
		info = append(info, strings.Join(nonEmpty(s.Model, ctx), " · "))
	}
	if c := present.CacheText(s, m.now); c != "" {
		ttl := s.Cache.TTL.String()
		if s.Cache.TTLAssumed {
			ttl += " (assumed)"
		}
		info = append(info, "cache "+c+" · ttl "+ttl)
	}
	for _, n := range s.Notes {
		info = append(info, "note: "+n)
	}
	b.WriteString(head + "\n")
	for _, l := range info {
		b.WriteString(sDim.Render(ansi.Truncate(l, w, "…")) + "\n")
	}
	if d := present.Detail(s); d != "" && (s.Group == core.GroupNeedsYou || s.Activity == model.Working) {
		b.WriteString(groupStyle(s.Group).Render(ansi.Truncate(d, w, "…")) + "\n")
	}
	b.WriteString("\n")
	used := strings.Count(b.String(), "\n")
	rest := h - used
	if rest < 1 {
		return b.String()
	}
	switch pm {
	case previewPane:
		b.WriteString(tailLines(m.previewText, w, rest))
	case previewConversation:
		b.WriteString(m.conversation(s, w, rest))
	}
	return b.String()
}

// tailLines returns the last n non-trailing-blank lines of text, truncated
// to width w (escape sequences preserved).
func tailLines(text string, w, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "") + "\x1b[0m"
	}
	return strings.Join(lines, "\n")
}

func (m *Model) conversation(s core.Session, w, h int) string {
	var parts []string
	if s.Summary != "" {
		parts = append(parts, lipgloss.NewStyle().Foreground(cGreen).Render("summary: ")+s.Summary, "")
	}
	if s.FirstPrompt != "" && s.FirstPrompt != s.LastPrompt {
		parts = append(parts, sHeader.Render("started with"))
		parts = append(parts, wrapTail(s.FirstPrompt, w, max(2, h/5))...)
		parts = append(parts, "")
	}
	if s.LastPrompt != "" {
		parts = append(parts, sHeader.Render("you asked"+agoSuffix(m.now, s.LastPromptAt)))
		parts = append(parts, wrapTail(s.LastPrompt, w, max(3, h/3))...)
		parts = append(parts, "")
	}
	if s.LastMessage != "" {
		parts = append(parts, sHeader.Render("last message"))
		parts = append(parts, wrapTail(s.LastMessage, w, h)...)
	}
	if len(parts) > h {
		parts = parts[:h]
	}
	return strings.Join(parts, "\n")
}

func agoSuffix(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " (" + present.Age(now.Sub(t)) + " ago)"
}

// wrapTail word-wraps text to width w and keeps the last n lines.
func wrapTail(text string, w, n int) []string {
	wrapped := lipgloss.NewStyle().Width(w).Render(strings.TrimSpace(text))
	lines := strings.Split(wrapped, "\n")
	if len(lines) > n {
		lines = append([]string{sDim.Render("…")}, lines[len(lines)-n+1:]...)
	}
	return lines
}
