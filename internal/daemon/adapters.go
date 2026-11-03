package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"cccp/internal/model"
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

// ClaudeSummarizer runs `claude -p` with no tools, no settings (so no
// plugins or hooks) and no session persistence, under the session's own
// profile: CLAUDE_CONFIG_DIR selects the credentials, so the summary is
// billed to that profile's account and the session's text goes only there
// (verified: with an empty config dir, claude -p refuses as not logged in).
// CCCP_INTERNAL=1 makes cccp's own hook a no-op should hooks load anyway.
type ClaudeSummarizer struct {
	Dir string // working directory for the child
}

// Summarize implements Summarizer.
func (c ClaudeSummarizer) Summarize(ctx context.Context, modelName string, prof model.Profile, systemPrompt, input string) (string, error) {
	if testing.Testing() {
		return "", errInTest
	}
	argv, env, err := summaryCommand(modelName, prof, systemPrompt, input, os.Environ())
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
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

// summaryCommand builds the summarizer's argv and environment. It refuses
// without a profile: the profile decides the account, and must never be
// left to whatever the daemon's environment happens to say.
//
// The profile is selected with model.Profile.Environ: the default profile by
// removing CLAUDE_CONFIG_DIR, any other by setting it.
func summaryCommand(modelName string, prof model.Profile, systemPrompt, input string, environ []string) (argv, env []string, err error) {
	if prof.Dir == "" {
		return nil, nil, errors.New("summary: no profile to run under")
	}
	argv = []string{"claude", "-p",
		"--model", modelName,
		"--no-session-persistence",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--tools", "",
		"--system-prompt", systemPrompt,
		input}
	env = prof.Environ(append(environ[:len(environ):len(environ)], "CCCP_INTERNAL=1"))
	return argv, env, nil
}

// withoutEnv drops a variable from an environment list.
func withoutEnv(env []string, key string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}
