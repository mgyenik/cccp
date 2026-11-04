// Package workstream records tmux sessions ("workstreams") so they can be
// restored after the tmux server is gone, and decides when one has really
// been closed. Pure: the record is derived from observations of the tmux
// server, and restoring is a plan of tmux commands for the shell to run.
//
// Design: docs/workstreams.md.
package workstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"cccp/internal/model"
)

// Grace is how long a session must stay gone from a server that is still
// running before its workstream is forgotten. tmux fires session-closed for
// sessions while the server shuts down (logout, reboot), and the sessions
// disappear one by one; the server is gone well within Grace, and then
// nothing is forgotten.
const Grace = 10 * time.Second

// Server identifies a tmux server: pane and session ids are only unique
// within one, and a restarted server reuses its socket.
type Server struct {
	PID    int    `json:"pid"`
	Start  int64  `json:"start"` // #{start_time}
	Socket string `json:"socket"`
}

// Known: the server is identified.
func (s Server) Known() bool { return s.PID != 0 && s.Start != 0 }

// Pane kinds.
const (
	KindShell        = "shell"
	KindConversation = "conversation"
)

// Conversation is the agent conversation a pane runs, enough to resume it.
type Conversation struct {
	Agent          model.Agent `json:"agent"`
	SessionID      string      `json:"session_id"`
	ConfigDir      string      `json:"config_dir,omitempty"`
	DefaultProfile bool        `json:"default_profile,omitempty"`
	Title          string      `json:"title,omitempty"`
}

// Pane is one recorded pane. Conv is set for KindConversation. (A value,
// not a pointer: a record is copied, compared and printed, and must not
// share or show addresses.)
type Pane struct {
	Cwd    string       `json:"cwd"`
	Kind   string       `json:"kind"`
	Conv   Conversation `json:"conversation,omitzero"`
	Active bool         `json:"active,omitempty"`
}

// Window is one recorded window.
type Window struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	// AutoName: tmux names the window after what runs in it
	// (automatic-rename); restoring then gives no name.
	AutoName bool   `json:"auto_name,omitempty"`
	Layout   string `json:"layout"`
	Active   bool   `json:"active,omitempty"`
	Panes    []Pane `json:"panes"`
}

// Workstream is one recorded tmux session.
type Workstream struct {
	Name     string    `json:"name"`
	ID       string    `json:"id"` // #{session_id} on Server
	Server   Server    `json:"server"`
	LastUsed time.Time `json:"last_used"`
	Windows  []Window  `json:"windows"`
	// Dormant: its server is gone; it waits for `cccp start`.
	Dormant bool `json:"dormant,omitempty"`
}

// Pending is a workstream seen gone from its still-running server, to be
// forgotten if it is still gone Grace later.
type Pending struct {
	Name   string    `json:"name"`
	ID     string    `json:"id"`
	Server Server    `json:"server"`
	Since  time.Time `json:"since"`
}

// Record is everything recorded, kept sorted by name.
type Record struct {
	Workstreams []Workstream `json:"workstreams"`
	Pending     []Pending    `json:"pending,omitempty"`
}

// LivePane is one pane of the running server, with what runs in it.
type LivePane struct {
	Pane model.TmuxPane
	// Conv is the conversation running in the pane (a live agent session
	// with a known id, or a restore placeholder for one); nil for anything
	// else.
	Conv *Conversation
}

// Live is the running tmux server as observed. OK false means it could not
// be read: nothing is concluded from that.
type Live struct {
	OK bool
	// Running: a server answered. OK and not Running means none is
	// running (every workstream is dormant).
	Running bool
	Server  Server
	Panes   []LivePane
}

// Observe updates the record from an observation of the running server.
//
//   - A session on the running server is recorded as it is now.
//   - A recorded workstream from another server (or with no server
//     running) is dormant, unless a running session has its name, which
//     replaces it.
//   - A recorded workstream from the running server whose session is gone
//     is pending; it is forgotten once it has been gone for Grace, and the
//     pending mark is dropped if the session comes back or the server
//     changes.
//
// When the server could not be read, the record is returned unchanged.
func Observe(rec Record, live Live, now time.Time) Record {
	if !live.OK {
		return rec.Clone()
	}
	running := map[string]Workstream{}
	if live.Running {
		for _, w := range fromPanes(live) {
			running[w.Name] = w
		}
	}
	liveIDs := map[string]bool{}
	for _, w := range running {
		liveIDs[w.ID] = true
	}
	var out Record
	for _, w := range running {
		out.Workstreams = append(out.Workstreams, w)
	}
	pendingByID := map[string]Pending{}
	for _, p := range rec.Pending {
		if live.Running && p.Server == live.Server {
			pendingByID[p.ID] = p
		}
	}
	for _, w := range rec.Workstreams {
		if _, taken := running[w.Name]; taken {
			continue // running now: recorded above (a rename keeps its id)
		}
		sameServer := live.Running && w.Server.Known() && w.Server == live.Server
		switch {
		case sameServer && liveIDs[w.ID]:
			continue // renamed: the running session is recorded under its new name
		case sameServer:
			p, ok := pendingByID[w.ID]
			if !ok {
				p = Pending{Name: w.Name, ID: w.ID, Server: w.Server, Since: now}
			}
			if now.Sub(p.Since) >= Grace {
				continue // closed: forgotten
			}
			out.Pending = append(out.Pending, p)
			w.Dormant = false
			out.Workstreams = append(out.Workstreams, w)
		default:
			w.Dormant = true
			out.Workstreams = append(out.Workstreams, w)
		}
	}
	out.sort()
	return out
}

// fromPanes groups the running server's panes into workstreams, windows by
// index and panes by pane index (the order a layout assigns panes in).
func fromPanes(live Live) []Workstream {
	type indexedPane struct {
		index int
		pane  Pane
	}
	type window struct {
		w     Window
		panes []indexedPane
	}
	type session struct {
		w       Workstream
		windows map[int]*window
	}
	byID := map[string]*session{}
	var order []string
	for _, lp := range live.Panes {
		p := lp.Pane
		s, ok := byID[p.SessionID]
		if !ok {
			s = &session{
				w:       Workstream{Name: p.Session, ID: p.SessionID, Server: live.Server, LastUsed: p.SessionActivity},
				windows: map[int]*window{},
			}
			byID[p.SessionID] = s
			order = append(order, p.SessionID)
		}
		win, ok := s.windows[p.WindowIndex]
		if !ok {
			win = &window{w: Window{Index: p.WindowIndex, Name: p.WindowName, AutoName: p.WindowAutoName,
				Layout: p.WindowLayout, Active: p.WindowActive}}
			s.windows[p.WindowIndex] = win
		}
		// Window attributes come with every pane; any one saying the window
		// is active is enough.
		win.w.Active = win.w.Active || p.WindowActive
		pane := Pane{Cwd: p.Path, Kind: KindShell, Active: p.PaneActive}
		if lp.Conv != nil {
			pane.Kind, pane.Conv = KindConversation, *lp.Conv
		}
		win.panes = append(win.panes, indexedPane{p.PaneIndex, pane})
	}
	var out []Workstream
	for _, id := range order {
		s := byID[id]
		var idx []int
		for i := range s.windows {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for _, i := range idx {
			win := s.windows[i]
			sort.SliceStable(win.panes, func(a, b int) bool { return win.panes[a].index < win.panes[b].index })
			for _, ip := range win.panes {
				win.w.Panes = append(win.w.Panes, ip.pane)
			}
			s.w.Windows = append(s.w.Windows, win.w)
		}
		out = append(out, s.w)
	}
	return out
}

func (r *Record) sort() {
	sort.Slice(r.Workstreams, func(i, j int) bool { return r.Workstreams[i].Name < r.Workstreams[j].Name })
	sort.Slice(r.Pending, func(i, j int) bool { return r.Pending[i].Name < r.Pending[j].Name })
}

// Clone returns a deep copy.
func (r Record) Clone() Record {
	var c Record
	for _, w := range r.Workstreams {
		w.Windows = slices.Clone(w.Windows)
		for i := range w.Windows {
			w.Windows[i].Panes = slices.Clone(w.Windows[i].Panes)
		}
		c.Workstreams = append(c.Workstreams, w)
	}
	c.Pending = slices.Clone(r.Pending)
	return c
}

// Equal: the records persist the same (their JSON is identical, which also
// compares times by instant, not by monotonic clock reading).
func (r Record) Equal(o Record) bool {
	a, errA := json.Marshal(r)
	b, errB := json.Marshal(o)
	return errA == nil && errB == nil && string(a) == string(b)
}

// Deadline is when the earliest pending removal is due ("" none).
func (r Record) Deadline() time.Time {
	var t time.Time
	for _, p := range r.Pending {
		if d := p.Since.Add(Grace); t.IsZero() || d.Before(t) {
			t = d
		}
	}
	return t
}

// Dormant returns the dormant workstreams, most recently used first.
func (r Record) Dormant() []Workstream {
	var out []Workstream
	for _, w := range r.Workstreams {
		if w.Dormant {
			out = append(out, w)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastUsed.After(out[j].LastUsed) })
	return out
}

// Find returns the workstream with the given name.
func (r Record) Find(name string) (Workstream, bool) {
	for _, w := range r.Workstreams {
		if w.Name == name {
			return w, true
		}
	}
	return Workstream{}, false
}

// Validate checks a new workstream's name. tmux would silently turn '.'
// and ':' into '_' (they separate window and pane in targets), so they are
// refused rather than changed behind your back.
func Validate(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("a workstream needs a name")
	case name != strings.TrimSpace(name):
		return errors.New("no spaces at the start or end of a name")
	case strings.ContainsAny(name, ".:"):
		return errors.New("tmux does not allow '.' or ':' in a name")
	case strings.IndexFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return errors.New("no control characters in a name")
	}
	return nil
}

// ResumeOption is the pane option marking a restore placeholder; its value
// is the conversation (EncodeTag), so the record keeps the conversation
// while the placeholder waits.
const ResumeOption = "@cccp_resume"

// EncodeTag and ParseTag store a conversation in ResumeOption.
func EncodeTag(c Conversation) string {
	b, _ := json.Marshal(c)
	return string(b)
}

func ParseTag(s string) (*Conversation, bool) {
	if s == "" {
		return nil, false
	}
	var c Conversation
	if json.Unmarshal([]byte(s), &c) != nil || c.SessionID == "" || c.Agent == "" {
		return nil, false
	}
	return &c, true
}

// Command is one tmux command of a restore plan. MarkResume: the command
// creates a pane (and prints its id: Args include -P -F #{pane_id}); the
// shell then sets ResumeOption on it to MarkResume. Cosmetic: the command
// only arranges what exists (layout, which window and pane are active); if
// it fails, the restore goes on.
type Command struct {
	Args       []string
	MarkResume string
	Cosmetic   bool
}

// Bases are tmux's base-index and pane-base-index, which decide the index
// a new window or pane gets.
type Bases struct{ Window, Pane int }

// Plan returns the tmux commands that recreate w, detached. bin is cccp's
// path, for conversation placeholders (cccp resume-pane). Targets use
// "=NAME" so they match the session exactly, not by prefix.
func Plan(w Workstream, bases Bases, bin string) []Command {
	if len(w.Windows) == 0 {
		return nil
	}
	sess := "=" + w.Name
	target := func(win int) string { return fmt.Sprintf("%s:%d", sess, win) }
	var cmds []Command
	for wi, win := range w.Windows {
		if len(win.Panes) == 0 {
			continue
		}
		first := win.Panes[0]
		if wi == 0 {
			args := []string{"new-session", "-d", "-s", w.Name}
			if x, y, ok := layoutSize(win.Layout); ok {
				args = append(args, "-x", strconv.Itoa(x), "-y", strconv.Itoa(y))
			}
			cmds = append(cmds, paneCommand(args, win, first, true, bin))
			if win.Index != bases.Window {
				cmds = append(cmds, Command{Args: []string{"move-window", "-s", target(bases.Window), "-t", target(win.Index)}})
			}
		} else {
			cmds = append(cmds, paneCommand([]string{"new-window", "-d", "-t", target(win.Index)}, win, first, true, bin))
		}
		for pi := 1; pi < len(win.Panes); pi++ {
			// Split the last pane made, so panes keep their order.
			last := fmt.Sprintf("%s.%d", target(win.Index), bases.Pane+pi-1)
			cmds = append(cmds, paneCommand([]string{"split-window", "-d", "-t", last}, win, win.Panes[pi], false, bin))
		}
		if len(win.Panes) > 1 && win.Layout != "" {
			cmds = append(cmds, Command{Args: []string{"select-layout", "-t", target(win.Index), win.Layout}, Cosmetic: true})
		}
		for pi, p := range win.Panes {
			if p.Active && len(win.Panes) > 1 {
				cmds = append(cmds, Command{Args: []string{"select-pane", "-t", fmt.Sprintf("%s.%d", target(win.Index), bases.Pane+pi)}, Cosmetic: true})
			}
		}
	}
	for _, win := range w.Windows {
		if win.Active {
			cmds = append(cmds, Command{Args: []string{"select-window", "-t", target(win.Index)}, Cosmetic: true})
		}
	}
	return cmds
}

// paneCommand completes a command that creates a pane: its directory, the
// window name (for the first pane of a window whose name was set by hand),
// and what runs in it.
func paneCommand(args []string, win Window, p Pane, firstOfWindow bool, bin string) Command {
	c := Command{}
	if p.Kind == KindConversation && p.Conv.SessionID != "" {
		args = append(args, "-P", "-F", "#{pane_id}")
		c.MarkResume = EncodeTag(p.Conv)
	}
	if p.Cwd != "" {
		args = append(args, "-c", p.Cwd)
	}
	if firstOfWindow && !win.AutoName && win.Name != "" {
		args = append(args, "-n", win.Name)
	}
	if c.MarkResume != "" {
		args = append(args, ResumeArgs(bin, p.Conv)...)
	}
	c.Args = args
	return c
}

// ResumeArgs is the placeholder command for a conversation.
func ResumeArgs(bin string, c Conversation) []string {
	args := []string{bin, "resume-pane", "--agent", string(c.Agent), "--id", c.SessionID}
	if c.ConfigDir != "" {
		args = append(args, "--config-dir", c.ConfigDir)
	}
	if c.DefaultProfile {
		args = append(args, "--default-profile")
	}
	if c.Title != "" {
		args = append(args, "--title", c.Title)
	}
	return args
}

var layoutSizeRe = regexp.MustCompile(`^[0-9a-f]{4},(\d+)x(\d+),`)

// layoutSize is the window size a layout was recorded at
// ("b25d,80x24,0,0,0" → 80, 24).
func layoutSize(layout string) (x, y int, ok bool) {
	m := layoutSizeRe.FindStringSubmatch(layout)
	if m == nil {
		return 0, 0, false
	}
	x, _ = strconv.Atoi(m[1])
	y, _ = strconv.Atoi(m[2])
	return x, y, x > 0 && y > 0
}
