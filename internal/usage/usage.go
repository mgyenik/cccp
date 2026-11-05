// Package usage parses the subscription usage limits Claude Code and agy
// report about themselves, from their own non-interactive commands (no
// model call, no tokens):
//
//   - claude -p "/usage" --output-format stream-json --verbose: the local
//     command's message carries usage_report.rate_limits.limits[], the
//     server's own rows (kind, percent used, reset, scope, severity), as
//     the status line gets them (Claude Code 2.1.318);
//   - agy -p "/usage" --output-format json: command.data.groups[].buckets[]
//     (window, remaining fraction, reset), agy 1.5.1.
//
// Pure: bytes in, limits out. The shell runs the commands.
package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Limit is one usage window of one account.
type Limit struct {
	Label    string    // "Current session", "Week (all models)", "Gemini · 5 hours"
	Used     float64   // percent of the window used, 0–100
	ResetsAt time.Time // zero if unknown
	// Severity is the server's reading ("normal", "warning", "critical");
	// "" when the source gives none (agy).
	Severity string
}

// Account is one account's limits, or why there are none.
type Account struct {
	Provider string // "Claude", "Antigravity"
	Name     string // the Claude profile ("" for agy)
	Limits   []Limit
	Note     string // e.g. extra usage, or "no subscription limits"
	Err      string
}

// ParseClaude reads claude -p "/usage" --output-format stream-json output.
func ParseClaude(out []byte) (limits []Limit, note string, err error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		var line struct {
			Report *struct {
				RateLimits *struct {
					Limits []struct {
						Kind     string       `json:"kind"`
						Percent  float64      `json:"percent"`
						ResetsAt *string      `json:"resets_at"`
						Severity string       `json:"severity"`
						Scope    *claudeScope `json:"scope"`
					} `json:"limits"`
					Extra *struct {
						Enabled  bool     `json:"is_enabled"`
						Limit    *float64 `json:"monthly_limit"`
						Used     *float64 `json:"used_credits"`
						Currency *string  `json:"currency"`
					} `json:"extra_usage"`
				} `json:"rate_limits"`
			} `json:"usage_report"`
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if line.IsError {
			return nil, "", errors.New(firstLine(line.Result))
		}
		if line.Report == nil {
			continue
		}
		rl := line.Report.RateLimits
		if rl == nil {
			return nil, "no subscription limits (an API key, or not signed in to a plan)", nil
		}
		for _, l := range rl.Limits {
			lim := Limit{Label: claudeLabel(l.Kind, l.Scope), Used: l.Percent, Severity: l.Severity}
			if l.ResetsAt != nil {
				lim.ResetsAt, _ = time.Parse(time.RFC3339Nano, *l.ResetsAt)
			}
			limits = append(limits, lim)
		}
		if e := rl.Extra; e != nil && e.Enabled {
			note = "extra usage on"
			if e.Used != nil && e.Limit != nil {
				cur := ""
				if e.Currency != nil {
					cur = " " + *e.Currency
				}
				note = fmt.Sprintf("extra usage: %.2f of %.2f%s this month", *e.Used, *e.Limit, cur)
			}
		}
		return limits, note, nil
	}
	return nil, "", errors.New("no usage report in claude's output")
}

type claudeScope struct {
	Model *struct {
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Surface *struct {
		DisplayName string `json:"display_name"`
	} `json:"surface"`
}

// claudeLabel names a row by its kind (never by a label the server might
// change), with its scope's display name.
func claudeLabel(kind string, sc *claudeScope) string {
	scope := ""
	if sc != nil {
		switch {
		case sc.Model != nil && sc.Model.DisplayName != "":
			scope = sc.Model.DisplayName
		case sc.Surface != nil && sc.Surface.DisplayName != "":
			scope = sc.Surface.DisplayName
		}
	}
	switch kind {
	case "session":
		return "Current session"
	case "weekly_all":
		return "Week (all models)"
	case "weekly_scoped":
		if scope != "" {
			return "Week (" + scope + ")"
		}
		return "Week (scoped)"
	}
	if scope != "" {
		return kind + " (" + scope + ")"
	}
	return kind
}

// ParseAgy reads agy -p "/usage" --output-format json output.
func ParseAgy(out []byte) ([]Limit, error) {
	var v struct {
		Status   string `json:"status"`
		Response string `json:"response"`
		Command  *struct {
			Data *struct {
				Groups []struct {
					Name    string `json:"name"`
					Buckets []struct {
						Name      string   `json:"name"`
						Window    string   `json:"window"`
						Remaining *float64 `json:"remaining_fraction"`
						ResetTime string   `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &v); err != nil {
		return nil, fmt.Errorf("agy's output: %v", err)
	}
	if v.Status != "SUCCESS" {
		msg := firstLine(v.Response)
		if msg == "" {
			msg = "status " + v.Status
		}
		return nil, errors.New(msg)
	}
	if v.Command == nil || v.Command.Data == nil {
		return nil, errors.New("no usage data in agy's output")
	}
	var limits []Limit
	for _, g := range v.Command.Data.Groups {
		for _, b := range g.Buckets {
			if b.Remaining == nil {
				continue // a disabled bucket
			}
			lim := Limit{Label: strings.TrimSuffix(g.Name, " Models") + " · " + agyWindow(b.Window, b.Name), Used: 100 * (1 - *b.Remaining)}
			lim.ResetsAt, _ = time.Parse(time.RFC3339Nano, b.ResetTime)
			limits = append(limits, lim)
		}
	}
	return limits, nil
}

func agyWindow(window, name string) string {
	switch window {
	case "weekly":
		return "week"
	case "5h":
		return "5 hours"
	}
	if window != "" {
		return window
	}
	return name
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
