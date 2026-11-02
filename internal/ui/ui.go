// Package ui is the cockpit: a Bubble Tea TUI over the engine snapshot.
package ui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"cccp/internal/core"
	"cccp/internal/model"
	"cccp/internal/present"
	"cccp/internal/search"
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
}

// Searcher is transcript full-text search (search.Index).
type Searcher interface {
	Update() (int, error)
	Run(q search.Query, limit int) ([]search.Hit, error)
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
}

type mode int

const (
	modeList mode = iota
	modeFilter
	modeSearchInput
	modeSearchResults
	modeAlias
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
	hits  []search.Hit
	err   error
	took  time.Duration
}
type doneMsg struct{ err error }

// Model is the Bubble Tea model.
type Model struct {
	opt Options

	snap    core.Snapshot
	hasSnap bool
	now     time.Time

	width, height int

	mode      mode
	cursor    int // index into visible()
	offset    int
	showEnded bool
	preview   previewMode

	filter textinput.Model
	query  textinput.Model
	alias  textinput.Model

	hits       []search.Hit
	hitCursor  int
	hitOffset  int
	searching  bool
	searchInfo string

	previewKey  string
	previewText string

	status    string
	statusAt  time.Time
	quitting  bool
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
	f.Placeholder = "filter by title, project, location, state…"
	q := textinput.New()
	q.Prompt = "? "
	q.Placeholder = `search transcripts (all words must match; "quote phrases")`
	a := textinput.New()
	a.Prompt = "alias: "
	return &Model{opt: opt, filter: f, query: q, alias: a, now: time.Now()}
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
	var out []core.Session
	f := strings.ToLower(strings.TrimSpace(m.filter.Value()))
	for _, s := range m.snap.Sessions {
		if s.Group == core.GroupEnded && !m.showEnded && f == "" {
			continue
		}
		if f != "" && !matches(s, f) {
			continue
		}
		out = append(out, s)
	}
	return out
}

func matches(s core.Session, f string) bool {
	hay := strings.ToLower(strings.Join([]string{
		s.DisplayTitle(), s.Title, s.Cwd, s.Location, s.TmuxSess, string(s.Agent),
		s.Group.String(), string(s.Activity), s.LastPrompt, s.Summary, s.SessionID,
	}, " "))
	for _, t := range strings.Fields(f) {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
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
		m.query.SetWidth(msg.Width - 4)
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

	case searchMsg:
		m.searching = false
		if msg.query != m.query.Value() {
			return m, nil
		}
		if msg.err != nil {
			m.searchInfo = "search failed: " + msg.err.Error()
			return m, nil
		}
		m.hits, m.hitCursor, m.hitOffset = msg.hits, 0, 0
		m.searchInfo = fmt.Sprintf("%d sessions match (%v)", len(msg.hits), msg.took.Round(time.Millisecond))
		m.mode = modeSearchResults
		return m, nil

	case doneMsg:
		if msg.err != nil {
			m.setStatus("error: " + msg.err.Error())
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit

	case tea.KeyPressMsg:
		return m.key(msg)
	}

	// Forward everything else (cursor blink etc.) to the focused input.
	var cmd tea.Cmd
	switch m.mode {
	case modeFilter:
		m.filter, cmd = m.filter.Update(msg)
	case modeSearchInput:
		m.query, cmd = m.query.Update(msg)
	case modeAlias:
		m.alias, cmd = m.alias.Update(msg)
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
	case modeFilter:
		switch key {
		case "esc":
			m.filter.SetValue("")
			m.filter.Blur()
			m.mode = modeList
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
		m.filter, cmd = m.filter.Update(k)
		m.cursor = 0
		m.rememberSelection()
		return m, tea.Batch(cmd, m.loadPreview())

	case modeSearchInput:
		switch key {
		case "esc":
			m.query.Blur()
			m.mode = modeList
			return m, nil
		case "enter":
			q := m.query.Value()
			if strings.TrimSpace(q) == "" {
				return m, nil
			}
			if m.opt.Search == nil {
				m.searchInfo = "search is not available"
				return m, nil
			}
			m.searching = true
			m.searchInfo = "searching…"
			return m, runSearch(m.opt.Search, q)
		}
		var cmd tea.Cmd
		m.query, cmd = m.query.Update(k)
		return m, cmd

	case modeSearchResults:
		switch key {
		case "esc", "q":
			m.mode = modeList
			return m, nil
		case "?", "/":
			m.mode = modeSearchInput
			return m, m.query.Focus()
		case "up", "k":
			if m.hitCursor > 0 {
				m.hitCursor--
			}
		case "down", "j":
			if m.hitCursor < len(m.hits)-1 {
				m.hitCursor++
			}
		case "enter":
			return m, m.openHit()
		}
		return m, nil

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
	case "?":
		m.mode = modeSearchInput
		return m, m.query.Focus()
	case "e":
		m.showEnded = !m.showEnded
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
	}
	return m, m.loadPreview()
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

// openHit jumps to a search hit's session: the live pane if it is running,
// otherwise a resumed session.
func (m *Model) openHit() tea.Cmd {
	if m.hitCursor >= len(m.hits) || m.opt.Actions.Jump == nil {
		return nil
	}
	h := m.hits[m.hitCursor]
	target := core.Session{Key: h.Doc.Key(), Agent: h.Doc.Agent, SessionID: h.Doc.SessionID, Cwd: h.Doc.Cwd, Title: h.Doc.Title}
	for _, s := range m.snap.Sessions {
		if s.Key == target.Key {
			target = s
		}
	}
	jump := m.opt.Actions.Jump
	return func() tea.Msg { return doneMsg{err: jump(target)} }
}

func runSearch(ix Searcher, q string) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		if _, err := ix.Update(); err != nil {
			return searchMsg{query: q, err: err}
		}
		hits, err := ix.Run(search.ParseQuery(q), 200)
		return searchMsg{query: q, hits: hits, err: err, took: time.Since(start)}
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

// loadPreview captures the selected pane, when the preview shows a pane.
func (m *Model) loadPreview() tea.Cmd {
	s, ok := m.selected()
	if !ok {
		m.previewKey, m.previewText = "", ""
		return nil
	}
	if s.Key != m.previewKey {
		m.previewKey, m.previewText = s.Key, ""
	}
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
	case core.GroupEnded, core.GroupUnknown:
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
	var body string
	if m.mode == modeSearchResults || (m.mode == modeSearchInput && len(m.hits) > 0) {
		body = m.renderHits(m.width, bodyH)
	} else {
		body = m.renderBody(bodyH)
	}
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m *Model) renderHeader() string {
	c := m.snap.Counts()
	var parts []string
	for _, g := range core.Groups {
		if g == core.GroupEnded {
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
	left := sTitle.Render("cccp") + "  " + strings.Join(parts, "  ")
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
	case modeFilter:
		line = m.filter.View()
	case modeSearchInput:
		line = m.query.View()
		if m.searchInfo != "" {
			line += "  " + sDim.Render(m.searchInfo)
		}
	case modeAlias:
		line = m.alias.View()
	case modeSearchResults:
		line = sKeyHint.Render("enter open/resume · ↑↓ move · ? new search · esc back") + "  " + sDim.Render(m.searchInfo)
	default:
		hint := "enter jump · tab next needing you · / filter · ? search · a alias · m seen · p preview · e ended · r rescan · q quit"
		if f := m.filter.Value(); f != "" {
			hint = "filter: " + f + " · " + hint
		}
		line = sKeyHint.Render(ansi.Truncate(hint, m.width, "…"))
	}
	if m.status != "" && time.Since(m.statusAt) < 4*time.Second {
		line = sStatus.Render(m.status) + "  " + line
	}
	return ansi.Truncate(line, m.width, "…")
}

func (m *Model) renderBody(h int) string {
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
	for i, s := range vis {
		if s.Group != last {
			lines = append(lines, line{groupStyle(s.Group).Render(s.Group.String()), -1})
			last = s.Group
		}
		lines = append(lines, line{m.renderRow(s, w, i == m.cursor), i})
	}
	if len(vis) == 0 {
		msg := "no sessions"
		if m.filter.Value() != "" {
			msg = "no sessions match the filter"
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

func (m *Model) renderRow(s core.Session, w int, cursor bool) string {
	g := present.Glyph(s)
	if s.Inferred {
		g += "~"
	} else {
		g += " "
	}
	loc := s.Location
	if loc == "" {
		loc = "-"
	}
	meta := strings.Join(nonEmpty(
		present.Since(m.now, s.ActivitySince),
		present.CacheText(s, m.now),
		present.Tokens(s.ContextTokens),
	), "  ")
	titleW := 34
	if w < 90 {
		titleW = 24
	}
	left := fmt.Sprintf(" %s %s %s %s ", g, present.AgentBadge(s.Agent), pad(loc, 10), pad(s.DisplayTitle(), titleW))
	detailW := w - ansi.StringWidth(left) - ansi.StringWidth(meta) - 2
	detail := ""
	if detailW > 8 {
		detail = pad(present.OneLine(present.Detail(s)), detailW)
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
	if s.Inferred {
		info[0] += " (inferred)"
	}
	if s.Cwd != "" {
		info = append(info, s.Cwd)
	}
	if s.Model != "" || s.ContextTokens > 0 {
		info = append(info, strings.Join(nonEmpty(s.Model, present.Tokens(s.ContextTokens)+" context"), " · "))
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

func (m *Model) renderHits(w, h int) string {
	if len(m.hits) == 0 {
		return sDim.Render("no matches")
	}
	per := 3
	visible := h / per
	if visible < 1 {
		visible = 1
	}
	if m.hitCursor < m.hitOffset {
		m.hitOffset = m.hitCursor
	}
	if m.hitCursor >= m.hitOffset+visible {
		m.hitOffset = m.hitCursor - visible + 1
	}
	live := map[string]core.Session{}
	for _, s := range m.snap.Sessions {
		live[s.Key] = s
	}
	var b strings.Builder
	for i := m.hitOffset; i < len(m.hits) && i < m.hitOffset+visible; i++ {
		hit := m.hits[i]
		state := "ended · resume"
		if s, ok := live[hit.Doc.Key()]; ok && s.Alive {
			state = s.Group.String() + " · " + s.Location
		}
		title := fmt.Sprintf(" %s %-5s %s", present.AgentBadge(hit.Doc.Agent), present.Since(m.now, hit.At), hit.Doc.Title)
		meta := fmt.Sprintf("%s  %d hits  %s", state, hit.Matches, hit.Doc.Cwd)
		l1 := pad(title, w-ansi.StringWidth(meta)-1) + " " + meta
		l2 := "     " + hit.Role + ": " + present.OneLine(hit.Snippet)
		if i == m.hitCursor {
			b.WriteString(sCursor.Render(ansi.Truncate(l1, w, "…")) + "\n")
		} else {
			b.WriteString(sTitle.Render(ansi.Truncate(l1, w, "…")) + "\n")
		}
		b.WriteString(sDim.Render(ansi.Truncate(l2, w, "…")) + "\n\n")
	}
	return lipgloss.NewStyle().Height(h).MaxHeight(h).Render(strings.TrimRight(b.String(), "\n"))
}
