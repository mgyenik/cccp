package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"cccp/internal/model"
	"cccp/internal/ui"
	planusage "cccp/internal/usage"
)

// usageTimeout bounds one account's usage command (each measured at about
// 2 s).
const usageTimeout = 30 * time.Second

// usageSources is every account the cockpit's usage screen shows: each
// Claude profile (its own account, so its own limits) and agy, if found.
func (a *app) usageSources() []ui.UsageSource {
	var out []ui.UsageSource
	for _, p := range a.profiles(a.config()) {
		p := p
		out = append(out, ui.UsageSource{Provider: "Claude", Name: p.Name, Fetch: func() planusage.Account { return claudeUsage(p) }})
	}
	if _, err := exec.LookPath("agy"); err == nil {
		out = append(out, ui.UsageSource{Provider: "Antigravity", Fetch: agyUsage})
	}
	return out
}

// claudeUsage runs Claude Code's own /usage, non-interactively, under the
// profile (no model call): no settings, plugins, MCP servers or tools, and
// no transcript, like the summarizer.
func claudeUsage(p model.Profile) planusage.Account {
	acct := planusage.Account{Provider: "Claude", Name: p.Name}
	env := p.Environ(append(os.Environ(), "CCCP_INTERNAL=1"))
	out, err := runUsage(env, "claude", "-p", "/usage", "--output-format", "stream-json", "--verbose",
		"--no-session-persistence", "--setting-sources", "", "--strict-mcp-config", "--tools", "")
	limits, note, perr := planusage.ParseClaude(out)
	switch {
	case perr == nil:
		acct.Limits, acct.Note = limits, note
	case err != nil:
		acct.Err = err.Error()
	default:
		acct.Err = perr.Error()
	}
	return acct
}

// agyUsage runs agy's own /usage, non-interactively (no agent, no model
// call).
func agyUsage() planusage.Account {
	acct := planusage.Account{Provider: "Antigravity"}
	out, err := runUsage(os.Environ(), "agy", "-p", "/usage", "--output-format", "json")
	limits, perr := planusage.ParseAgy(out)
	switch {
	case perr == nil:
		acct.Limits = limits
	case err != nil:
		acct.Err = err.Error()
	default:
		acct.Err = perr.Error()
	}
	return acct
}

// runUsage runs a usage command in a neutral directory (not a project:
// neither CLI should take it as a workspace) and returns its output; on
// failure the error carries stderr's first line.
func runUsage(env []string, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), usageTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.Dir = env, os.TempDir()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return out, errors.New(name + ": no answer within " + usageTimeout.String())
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		if msg == "" {
			msg = err.Error()
		}
		return out, errors.New(name + ": " + msg)
	}
	return out, nil
}
