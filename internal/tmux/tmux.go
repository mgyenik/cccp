// Package tmux runs tmux commands. Every call is a single exec; nothing here
// polls.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Client talks to one tmux server.
type Client struct {
	// Socket is passed as -S when set; otherwise tmux's default server is
	// used (or $TMUX's when running inside tmux).
	Socket string
}

// Default returns a client for the user's default tmux server. A daemon not
// started from tmux has no $TMUX, so the default socket path is used.
func Default() *Client {
	if s := os.Getenv("CCCP_TMUX_SOCKET"); s != "" {
		return &Client{Socket: s}
	}
	return &Client{}
}

// ErrNoServer is returned when no tmux server is running.
var ErrNoServer = errors.New("no tmux server")

func (c *Client) args(a ...string) []string {
	if c.Socket != "" {
		return append([]string{"-S", c.Socket}, a...)
	}
	return a
}

// Run executes a tmux command and returns stdout.
func (c *Client) Run(a ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", c.args(a...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if strings.Contains(msg, "no server running") || strings.Contains(msg, "error connecting") {
			return "", ErrNoServer
		}
		return "", fmt.Errorf("tmux %s: %v: %s", strings.Join(a, " "), err, msg)
	}
	return out.String(), nil
}

// Pane is one tmux pane.
type Pane struct {
	ID          string // %N
	PID         int    // pane_pid (usually the shell)
	Session     string
	WindowID    string // @N
	WindowIndex int
	WindowName  string
	PaneIndex   int
	Title       string
	Command     string
	Path        string
	// Visible: the pane's window is the current window of a session that
	// has at least one attached client.
	Visible bool
	// WindowActivity is the last time the pane's window produced output.
	WindowActivity time.Time
	WindowPanes    int
}

// Location is "session:window" (with ".pane" when the window is split).
func (p Pane) Location() string {
	s := fmt.Sprintf("%s:%d", p.Session, p.WindowIndex)
	if p.WindowPanes > 1 {
		s += fmt.Sprintf(".%d", p.PaneIndex)
	}
	return s
}

// sep separates fields in tmux format output. tmux escapes control
// characters (\x1f becomes "\\037"), so a printable character that cannot
// plausibly occur in names or titles is used: U+241F SYMBOL FOR UNIT SEPARATOR.
const sep = "␟"

var paneFormat = strings.Join([]string{
	"#{pane_id}", "#{pane_pid}", "#{session_name}", "#{window_id}", "#{window_index}",
	"#{window_name}", "#{pane_index}", "#{pane_title}", "#{pane_current_command}",
	"#{pane_current_path}", "#{window_active}", "#{session_attached}",
	"#{window_activity}", "#{window_panes}",
}, sep)

// ListPanes returns every pane on the server.
func (c *Client) ListPanes() ([]Pane, error) {
	out, err := c.Run("list-panes", "-a", "-F", paneFormat)
	if err != nil {
		return nil, err
	}
	var panes []Pane
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Split(line, sep)
		if len(f) != 14 {
			continue
		}
		p := Pane{
			ID: f[0], Session: f[2], WindowID: f[3], WindowName: f[5],
			Title: f[7], Command: f[8], Path: f[9],
		}
		p.PID, _ = strconv.Atoi(f[1])
		p.WindowIndex, _ = strconv.Atoi(f[4])
		p.PaneIndex, _ = strconv.Atoi(f[6])
		attached, _ := strconv.Atoi(f[11])
		p.Visible = f[10] == "1" && attached > 0
		if ts, err := strconv.ParseInt(f[12], 10, 64); err == nil && ts > 0 {
			p.WindowActivity = time.Unix(ts, 0)
		}
		p.WindowPanes, _ = strconv.Atoi(f[13])
		panes = append(panes, p)
	}
	return panes, nil
}

// ClientInfo is one attached client.
type ClientInfo struct {
	Name    string
	Session string
	Pane    string // active pane of the client's current window
	Focused bool   // terminal focus (needs focus-events on); see FocusKnown
	// FocusKnown is false when tmux cannot know terminal focus (focus-events
	// off), in which case Focused is assumed true.
	FocusKnown bool
}

// ListClients returns attached clients.
func (c *Client) ListClients() ([]ClientInfo, error) {
	out, err := c.Run("list-clients", "-F", strings.Join([]string{"#{client_name}", "#{client_session}", "#{pane_id}", "#{client_flags}"}, sep))
	if err != nil {
		return nil, err
	}
	focusEvents := c.focusEventsOn()
	var cs []ClientInfo
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Split(line, sep)
		if len(f) != 4 {
			continue
		}
		ci := ClientInfo{Name: f[0], Session: f[1], Pane: f[2], FocusKnown: focusEvents}
		ci.Focused = !focusEvents || hasFlag(f[3], "focused")
		cs = append(cs, ci)
	}
	return cs, nil
}

func hasFlag(flags, want string) bool {
	for _, f := range strings.Split(flags, ",") {
		if f == want {
			return true
		}
	}
	return false
}

func (c *Client) focusEventsOn() bool {
	out, err := c.Run("show-options", "-gv", "focus-events")
	return err == nil && strings.TrimSpace(out) == "on"
}

// SetGlobalOption sets (or with an empty value, unsets) a global user option.
func (c *Client) SetGlobalOption(name, value string) error {
	if value == "" {
		_, err := c.Run("set-option", "-gqu", name)
		return err
	}
	_, err := c.Run("set-option", "-gq", name, value)
	return err
}

// SetWindowOption sets (or unsets) a window user option.
func (c *Client) SetWindowOption(window, name, value string) error {
	if value == "" {
		_, err := c.Run("set-option", "-wqu", "-t", window, name)
		return err
	}
	_, err := c.Run("set-option", "-wq", "-t", window, name, value)
	return err
}

// SwitchClient makes client show target (a pane id). With an empty client,
// tmux picks the current client.
func (c *Client) SwitchClient(client, target string) error {
	a := []string{"switch-client"}
	if client != "" {
		a = append(a, "-c", client)
	}
	a = append(a, "-t", target)
	if _, err := c.Run(a...); err != nil {
		return err
	}
	// switch-client selects the window; make sure the pane is active too.
	_, err := c.Run("select-pane", "-t", target)
	return err
}

// CapturePane returns the visible contents of a pane, with escape sequences
// when ansi is true.
func (c *Client) CapturePane(pane string, ansi bool, lines int) (string, error) {
	a := []string{"capture-pane", "-p", "-t", pane}
	if ansi {
		a = append(a, "-e")
	}
	if lines > 0 {
		a = append(a, "-S", fmt.Sprintf("-%d", lines))
	}
	return c.Run(a...)
}

// NewWindow opens a window in session running cmd in dir and returns its
// pane id.
func (c *Client) NewWindow(session, dir, name string, cmd []string) (string, error) {
	a := []string{"new-window", "-P", "-F", "#{pane_id}"}
	if session != "" {
		a = append(a, "-t", session+":")
	}
	if dir != "" {
		a = append(a, "-c", dir)
	}
	if name != "" {
		a = append(a, "-n", name)
	}
	a = append(a, cmd...)
	out, err := c.Run(a...)
	return strings.TrimSpace(out), err
}

// DisplayMessage shows a message on a client's status line.
func (c *Client) DisplayMessage(client, msg string) error {
	a := []string{"display-message"}
	if client != "" {
		a = append(a, "-c", client)
	}
	_, err := c.Run(append(a, msg)...)
	return err
}

// ShellQuote quotes s for tmux command strings run by /bin/sh.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
