package core

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Summary verdicts: what a live session's summary says the user has to do
// (SummaryRecord.Verdict, Session.SummaryVerdict).
const (
	VerdictAct  = "act"  // something to do now: answer, decide, review, test
	VerdictWait = "wait" // nothing yet: a progress report, waiting on work
)

// SummaryInput is the summarizer's input for a session as it is now, and
// its hash: a summary stored under that hash describes this state, and no
// other. A closed conversation is described as a whole; a live session by
// what it needs from the user next.
func SummaryInput(s Session) (input, hash string) {
	var b strings.Builder
	if s.Group == GroupClosed {
		b.WriteString("Conversation title: " + s.DisplayTitle() + "\n")
		b.WriteString("User's first prompt:\n" + tail(s.FirstPrompt, 1200) + "\n")
		b.WriteString("User's last prompt:\n" + tail(s.LastPrompt, 1200) + "\n")
		b.WriteString("Assistant's last message (end):\n" + tail(s.LastMessage, 2000) + "\n")
		input = b.String()
		return input, hashOf("closed\n" + input)
	}
	b.WriteString("Session title: " + s.DisplayTitle() + "\n")
	if s.Question != "" {
		b.WriteString("It is waiting on: " + s.Question + "\n")
	}
	if s.Tool != "" && s.Group == GroupNeedsYou {
		b.WriteString("Pending tool: " + s.Tool + " " + s.ToolDetail + "\n")
	}
	// A turn Claude Code started on a background report answers that
	// report, not the user's last prompt (which can be hours old, and was
	// measured to make the summary answer it instead).
	if s.AutoTurn {
		b.WriteString("This turn was not started by the user: a background task or sub-agent reported, and the assistant responded to that report.\n")
	} else {
		b.WriteString("User's last prompt:\n" + tail(s.LastPrompt, 1500) + "\n")
	}
	if s.BackgroundShell {
		b.WriteString("A background shell command the assistant started is still running (a build, a test run, or a server left running for the user).\n")
	}
	b.WriteString("Assistant's last message:\n" + tail(s.LastMessage, 6000) + "\n")
	input = b.String()
	return input, hashOf(input)
}

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// tail is the last n bytes of s (not splitting a UTF-8 sequence), marked
// with "…" when cut.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut++
	}
	return "…" + s[cut:]
}
