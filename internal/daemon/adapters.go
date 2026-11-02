package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"

	"cccp/internal/tmux"
)

// Real adapters for the daemon's ports. Each refuses to act inside a test
// binary: a test that forgets to install a fake fails instead of touching
// the user's desktop, tmux server or Claude subscription.

var errInTest = errors.New("refusing to act on the real system from a test binary")

// TmuxOptions sets cccp's user options on a tmux server.
type TmuxOptions struct{ Client *tmux.Client }

// SetStatus sets (or unsets) the global @cccp_status option.
func (t TmuxOptions) SetStatus(value string) error {
	if testing.Testing() && t.Client.Socket == "" {
		return errInTest
	}
	return t.Client.SetGlobalOption("@cccp_status", value)
}

// SetGlyph sets (or unsets) a window's @cccp_glyph option.
func (t TmuxOptions) SetGlyph(window, value string) error {
	if testing.Testing() && t.Client.Socket == "" {
		return errInTest
	}
	return t.Client.SetWindowOption(window, "@cccp_glyph", value)
}

// ExecNotifier runs the notification command without waiting for it.
type ExecNotifier struct{}

// Notify implements Notifier.
func (ExecNotifier) Notify(argv []string) error {
	if testing.Testing() {
		return errInTest
	}
	if len(argv) == 0 {
		return errors.New("empty notify command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// SummarySystemPrompt instructs the summary model.
const SummarySystemPrompt = "You write one-line status lines for a dashboard of coding-agent sessions. " +
	"Given a session's title, the user's last prompt and the assistant's last message, say in one line " +
	"under 90 characters what the session needs from the user next (a decision, an answer, a review, " +
	"or nothing). No preamble, no quotes, no trailing period."

// ClaudeSummarizer runs `claude -p` with no tools, no settings (so no
// plugins or hooks) and no session persistence. CCCP_INTERNAL=1 makes cccp's
// own hook a no-op should hooks load anyway.
type ClaudeSummarizer struct {
	Dir string // working directory for the child
}

// Summarize implements Summarizer.
func (c ClaudeSummarizer) Summarize(ctx context.Context, model, input string) (string, error) {
	if testing.Testing() {
		return "", errInTest
	}
	cmd := exec.CommandContext(ctx, "claude", "-p",
		"--model", model,
		"--no-session-persistence",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--tools", "",
		"--system-prompt", SummarySystemPrompt,
		input)
	cmd.Env = append(os.Environ(), "CCCP_INTERNAL=1")
	cmd.Stdin = nil // /dev/null: claude -p otherwise waits for stdin
	if c.Dir != "" {
		if err := os.MkdirAll(c.Dir, 0o700); err == nil {
			cmd.Dir = c.Dir
		}
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}
