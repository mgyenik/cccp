package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"time"

	"cccp/internal/engine"
	"cccp/internal/paths"
)

// summaryJob asks for a one-line "what does this session need" summary.
type summaryJob struct {
	key   string
	hash  string
	input string
}

type summaryResult struct {
	key  string
	rec  engine.SummaryRecord
	err  error
	took time.Duration
}

const summarySystemPrompt = "You write one-line status lines for a dashboard of coding-agent sessions. " +
	"Given a session's title, the user's last prompt and the assistant's last message, say in one line " +
	"under 90 characters what the session needs from the user next (a decision, an answer, a review, " +
	"or nothing). No preamble, no quotes, no trailing period."

// summaryInput builds the prompt and its hash for a session.
func summaryInput(s engine.Session) (string, string) {
	var b strings.Builder
	b.WriteString("Session title: " + s.DisplayTitle() + "\n")
	if s.Question != "" {
		b.WriteString("It is waiting on: " + s.Question + "\n")
	}
	if s.Tool != "" && s.Group == engine.GroupNeedsYou {
		b.WriteString("Pending tool: " + s.Tool + " " + s.ToolDetail + "\n")
	}
	b.WriteString("User's last prompt:\n" + tail(s.LastPrompt, 1500) + "\n")
	b.WriteString("Assistant's last message (end):\n" + tail(s.LastMessage, 2500) + "\n")
	in := b.String()
	h := sha256.Sum256([]byte(in))
	return in, hex.EncodeToString(h[:8])
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// runSummary runs one job with `claude -p`. CCCP_INTERNAL=1 makes cccp's own
// hooks ignore this process, and it loads no settings, tools or MCP servers.
func runSummary(job summaryJob, model string) summaryResult {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p",
		"--model", model,
		"--no-session-persistence",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--tools", "",
		"--system-prompt", summarySystemPrompt,
		job.input)
	cmd.Env = append(os.Environ(), "CCCP_INTERNAL=1")
	cmd.Stdin = nil
	if err := os.MkdirAll(paths.CacheDir(), 0o700); err == nil {
		cmd.Dir = paths.CacheDir()
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	res := summaryResult{key: job.key, took: time.Since(start), err: err}
	if err == nil {
		line := strings.TrimSpace(out.String())
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		line = strings.Trim(line, `"`)
		if r := []rune(line); len(r) > 120 {
			line = string(r[:119]) + "…"
		}
		res.rec = engine.SummaryRecord{InputHash: job.hash, Text: line, At: time.Now()}
	}
	return res
}
