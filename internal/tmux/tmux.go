// Package tmux runs tmux commands. Every call is a single exec; nothing here
// polls.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cccp/internal/model"
	"cccp/internal/workstream"
)

// Client talks to one tmux server.
type Client struct {
	// Socket is passed as -S when set; otherwise tmux's default server is
	// used (or $TMUX's when running inside tmux).
	Socket string

	mu         sync.Mutex
	socketPath string // the server's socket path, once known
}

// New returns a client for the server at socket, or for tmux's default
// server when socket is empty.
func New(socket string) *Client { return &Client{Socket: socket} }

// ErrNoServer is returned when no tmux server is running.
var ErrNoServer = errors.New("no tmux server")

func (c *Client) args(a ...string) []string {
	if c.Socket != "" {
		return append([]string{"-S", c.Socket}, a...)
	}
	return a
}

// Run executes a tmux command and returns stdout.
func (c *Client) Run(a ...string) (string, error) { return c.runWithTimeout(5*time.Second, a...) }

// runWithTimeout runs a tmux command; d == 0 means no timeout (for commands
// that wait on the user, such as display-popup).
func (c *Client) runWithTimeout(d time.Duration, a ...string) (string, error) {
	if c.Socket == "" && testing.Testing() {
		// Tests must name an isolated server (tmux -L / -S); never the
		// user's default one.
		return "", errors.New("refusing to talk to the default tmux server from a test binary")
	}
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if d > 0 {
		ctx, cancel = context.WithTimeout(ctx, d)
	}
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

// sep separates fields in tmux format output. tmux escapes control
// characters (\x1f becomes "\\037"), so a printable character that cannot
// plausibly occur in names or titles is used: U+241F SYMBOL FOR UNIT SEPARATOR.
// Pane and ClientInfo are plain data, defined in model so the functional
// core can use them without depending on this package.
type (
	Pane       = model.TmuxPane
	ClientInfo = model.TmuxClient
)

const sep = "␟"

var paneFormat = strings.Join([]string{
	"#{pane_id}", "#{pane_pid}", "#{session_name}", "#{window_id}", "#{window_index}",
	"#{window_name}", "#{pane_index}", "#{pane_title}", "#{pane_current_command}",
	"#{pane_current_path}", "#{window_active}", "#{session_attached}",
	"#{window_activity}", "#{window_panes}",
	"#{session_id}", "#{session_activity}", "#{window_layout}", "#{automatic-rename}", "#{pane_active}",
	"#{@cccp_resume}", "#{pid}", "#{start_time}",
}, sep)

// paneFields is the number of fields in paneFormat.
const paneFields = 22

// ListPanes returns every pane on the server.
func (c *Client) ListPanes() ([]Pane, error) {
	out, err := c.Run("list-panes", "-a", "-F", paneFormat)
	if err != nil {
		if strings.Contains(err.Error(), "no current target") {
			// A running server with no session (exit-empty off): no panes.
			return nil, nil
		}
		return nil, err
	}
	return ParsePanes(out), nil
}

// Bases returns base-index and pane-base-index (which decide a new window's
// and pane's index), starting the server if none runs: the values come from
// the user's tmux.conf, loaded when the server starts. One invocation, so
// the server is up while asked even with exit-empty on.
func (c *Client) Bases() (workstream.Bases, error) {
	out, err := c.Run("start-server", ";", "show-options", "-gv", "base-index", ";", "show-options", "-gwv", "pane-base-index")
	if err != nil {
		return workstream.Bases{}, err
	}
	return ParseBases(out)
}

// ParseBases parses Bases' output: the two values, one per line.
func ParseBases(out string) (workstream.Bases, error) {
	f := strings.Fields(out)
	if len(f) != 2 {
		return workstream.Bases{}, fmt.Errorf("tmux base indexes: unexpected %q", out)
	}
	w, err1 := strconv.Atoi(f[0])
	p, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return workstream.Bases{}, fmt.Errorf("tmux base indexes: unexpected %q", out)
	}
	return workstream.Bases{Window: w, Pane: p}, nil
}

// Restore runs a restore plan (workstream.Plan), marking each placeholder
// pane it creates with workstream.ResumeOption. A cosmetic command that
// fails (a layout tmux rejects) is passed over; any other failure stops the
// restore, as the rest of the plan targets what failed to appear.
func (c *Client) Restore(cmds []workstream.Command) error {
	for _, cmd := range cmds {
		out, err := c.Run(cmd.Args...)
		if err != nil {
			if cmd.Cosmetic {
				continue
			}
			return err
		}
		if cmd.MarkResume == "" {
			continue
		}
		pane := strings.TrimSpace(out)
		if !strings.HasPrefix(pane, "%") {
			return fmt.Errorf("tmux %s: expected a pane id, got %q", cmd.Args[0], out)
		}
		if _, err := c.Run("set-option", "-p", "-t", pane, workstream.ResumeOption, cmd.MarkResume); err != nil {
			return err
		}
	}
	return nil
}

// ServerIdentity is the server's pid and start time, which with the socket
// identify it. list-panes carries them with every pane; this is for a server
// with no panes.
func (c *Client) ServerIdentity() (pid int, start int64, err error) {
	out, err := c.Run("display-message", "-p", "#{pid}"+sep+"#{start_time}")
	if err != nil {
		return 0, 0, err
	}
	a, b, _ := strings.Cut(strings.TrimSpace(out), sep)
	pid, _ = strconv.Atoi(a)
	start, _ = strconv.ParseInt(b, 10, 64)
	return pid, start, nil
}

// ParsePanes parses list-panes output in paneFormat.
func ParsePanes(out string) []Pane {
	var panes []Pane
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Split(line, sep)
		if len(f) != paneFields {
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
		p.SessionID, p.SessionActivity = f[14], unixTime(f[15])
		p.WindowActive, p.WindowLayout, p.WindowAutoName = f[10] == "1", f[16], f[17] == "1"
		p.PaneActive, p.Resume = f[18] == "1", f[19]
		p.ServerPID, _ = strconv.Atoi(f[20])
		p.ServerStart, _ = strconv.ParseInt(f[21], 10, 64)
		panes = append(panes, p)
	}
	return panes
}

// FocusSinceOption records when the server started asking terminals for
// focus reports (see model.TmuxClient.FocusReported).
const FocusSinceOption = "@cccp_focus_since"

// ClientFormat is list-clients' format for ParseClients. The server-wide
// focus-events and FocusSinceOption come with every client, so one command
// reads everything.
var ClientFormat = strings.Join([]string{
	"#{client_name}", "#{client_session}", "#{pane_id}", "#{client_flags}",
	"#{client_created}", "#{focus-events}", "#{" + FocusSinceOption + "}",
}, sep)

// ListClients returns attached clients.
func (c *Client) ListClients() ([]ClientInfo, error) {
	out, err := c.Run("list-clients", "-F", ClientFormat)
	if err != nil {
		return nil, err
	}
	return ParseClients(out), nil
}

// ParseClients parses list-clients output in ClientFormat.
func ParseClients(out string) []ClientInfo {
	var cs []ClientInfo
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Split(line, sep)
		if len(f) != 7 {
			continue
		}
		ci := ClientInfo{Name: f[0], Session: f[1], Pane: f[2], Focused: hasFlag(f[3], "focused"),
			Created: unixTime(f[4])}
		if f[5] == "1" { // focus reporting means nothing while focus-events is off
			ci.FocusSince = unixTime(f[6])
		}
		cs = append(cs, ci)
	}
	return cs
}

// unixTime parses tmux's seconds-since-epoch times; zero when empty or bad.
func unixTime(s string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func hasFlag(flags, want string) bool {
	for _, f := range strings.Split(flags, ",") {
		if f == want {
			return true
		}
	}
	return false
}

// GlobalOption reads a global user option ("" when unset).
func (c *Client) GlobalOption(name string) (string, error) {
	out, err := c.Run("show-options", "-gqv", name)
	return strings.TrimSpace(out), err
}

// SocketPath is the server's socket path: what identifies the server,
// since pane ids (%3) are only unique within one. A Client always talks to
// the same socket, so the path is asked for once (once the server runs).
func (c *Client) SocketPath() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.socketPath != "" {
		return c.socketPath, nil
	}
	out, err := c.Run("display-message", "-p", "#{socket_path}")
	if err != nil {
		return "", err
	}
	c.socketPath = strings.TrimSpace(out)
	return c.socketPath, nil
}

// EnsureFocusSince sets FocusSinceOption if it is not set, as decided by
// model.FocusSince. Run before focus-events is turned on.
func (c *Client) EnsureFocusSince(now time.Time) error {
	if cur, err := c.GlobalOption(FocusSinceOption); err != nil || cur != "" {
		return err
	}
	// While the server starts (the snippet loading from tmux.conf) there is
	// no session yet, and list-clients fails ("no current target"); no
	// session means no client attached.
	out, err := c.Run("display-message", "-p", "#{server_sessions}"+sep+"#{start_time}")
	if err != nil {
		return err
	}
	sessions, start, _ := strings.Cut(strings.TrimSpace(out), sep)
	attached := 0
	if sessions != "0" {
		names, err := c.Run("list-clients", "-F", "#{client_name}")
		if err != nil {
			return err
		}
		attached = len(strings.Fields(names))
	}
	since := model.FocusSince(attached, unixTime(start), now)
	return c.SetGlobalOption(FocusSinceOption, strconv.FormatInt(since.Unix(), 10))
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

// NewWindow opens a window in session running cmd in dir, with env
// (KEY=VALUE) set for the command, and returns its pane id. detached: it
// does not become the session's current window (nobody viewing the session
// is moved to it).
func (c *Client) NewWindow(session, dir, name string, env, cmd []string, detached bool) (string, error) {
	a := []string{"new-window", "-P", "-F", "#{pane_id}"}
	if detached {
		a = append(a, "-d")
	}
	for _, kv := range env {
		a = append(a, "-e", kv)
	}
	if session != "" {
		a = append(a, "-t", "="+session+":") // exactly this session, not one it prefixes
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

// Popup describes a display-popup.
type Popup struct {
	Client        string // client to show it on (required: see PopupArgs)
	Width, Height string // e.g. "92%"
	Title         string
	Env           []string // KEY=VALUE pairs for the popup's command
	Argv          []string // command to run; the popup closes when it exits
}

// PopupArgs builds the display-popup arguments. The client is passed
// explicitly because tmux (3.4) neither expands formats in a popup's
// command nor reliably resolves "the current client" from inside a popup
// when several clients are attached: the caller must expand
// #{client_name} beforehand (run-shell does).
func PopupArgs(p Popup) []string {
	a := []string{"display-popup", "-E", "-c", p.Client}
	if p.Width != "" {
		a = append(a, "-w", p.Width)
	}
	if p.Height != "" {
		a = append(a, "-h", p.Height)
	}
	if p.Title != "" {
		a = append(a, "-T", p.Title)
	}
	for _, kv := range p.Env {
		a = append(a, "-e", kv)
	}
	quoted := make([]string, len(p.Argv))
	for i, s := range p.Argv {
		quoted[i] = ShellQuote(s)
	}
	return append(a, strings.Join(quoted, " "))
}

// DisplayPopup opens a popup and waits until it closes.
func (c *Client) DisplayPopup(p Popup) error {
	if p.Client == "" {
		return errors.New("display-popup: no client")
	}
	_, err := c.runWithTimeout(0, PopupArgs(p)...)
	return err
}
