// Package hook turns an agent's hook invocation (JSON on stdin) into a
// normalised event and records it in the session's state file.
//
// Contract with the agents: never fail, never block, never print anything
// that could be read as a decision. The caller (cmd/cccp) always exits 0 and
// only logs Handle's errors.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"cccp/internal/model"
	"cccp/internal/procs"
	"cccp/internal/reduce"
)

// Limits on stored text, in bytes.
const (
	maxDetail   = 300
	maxQuestion = 600
	maxPrompt   = 2000
	maxMessage  = 4000
	maxStdin    = 64 << 20
)

// Updater applies a reduction to a session's stored state (store.Store).
type Updater interface {
	Update(agent model.Agent, sessionID string, fn func(model.HookState) model.HookState) error
}

// Agent identifies the agent process that ran the hook.
type Agent struct {
	PID     int
	Start   uint64
	Cmdline []string
}

// Deps are the hook's dependencies on its environment.
type Deps struct {
	Getenv func(string) string
	// FindAgent walks up from the hook's parent to the agent process
	// (claude or agy); ok is false when it cannot be found.
	FindAgent func(agent model.Agent) (Agent, bool)
	Store     Updater
}

// Handle processes one hook invocation for agent, reading the payload from
// r. now must be taken as early as possible in the process.
func Handle(d Deps, agent model.Agent, r io.Reader, now time.Time) error {
	if d.Getenv("CCCP_INTERNAL") == "1" {
		_, _ = io.Copy(io.Discard, r)
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r, maxStdin))
	if err != nil {
		return err
	}
	var ev model.Event
	switch agent {
	case model.Claude:
		ev, err = ParseClaude(raw)
	case model.Agy:
		ev, err = ParseAgy(raw, d.Getenv(AgyEventEnv))
	default:
		return fmt.Errorf("unknown agent %q", agent)
	}
	if err != nil {
		return err
	}
	if ev.SessionID == "" {
		return fmt.Errorf("%s %s: no session id in payload", agent, ev.Name)
	}
	ev.Time = now
	ev.Pane = d.Getenv("TMUX_PANE")
	ev.TmuxSocket = model.TmuxSocketFromEnv(d.Getenv("TMUX")) // kept only with a pane (reduce)

	if agent == model.Claude {
		// The hook inherits the agent's environment: this names the
		// session's profile ("" = the default, ~/.claude).
		ev.ConfigDir = d.Getenv("CLAUDE_CONFIG_DIR")
	}
	if a, ok := d.FindAgent(agent); ok {
		ev.PID, ev.PIDStart = a.PID, a.Start
		ev.PrintMode = model.IsPrintMode(a.Cmdline)
	}
	return d.Store.Update(agent, ev.SessionID, func(s model.HookState) model.HookState {
		return reduce.Apply(s, ev)
	})
}

// SystemFindAgent finds the agent process among the ancestors of ppid.
func SystemFindAgent(ppid int) func(model.Agent) (Agent, bool) {
	return func(agent model.Agent) (Agent, bool) {
		st, ok := procs.FindAncestor(ppid, string(agent), 8)
		if !ok {
			return Agent{}, false
		}
		return Agent{PID: st.PID, Start: st.StartTime, Cmdline: procs.Cmdline(st.PID)}, true
	}
}

// claudePayload covers the fields cccp reads from every Claude Code hook event.
type claudePayload struct {
	SessionID        string          `json:"session_id"`
	TranscriptPath   string          `json:"transcript_path"`
	Cwd              string          `json:"cwd"`
	PermissionMode   string          `json:"permission_mode"`
	HookEventName    string          `json:"hook_event_name"`
	AgentID          string          `json:"agent_id"`
	Source           string          `json:"source"`
	Model            string          `json:"model"`
	SessionTitle     string          `json:"session_title"`
	Prompt           string          `json:"prompt"`
	ToolName         string          `json:"tool_name"`
	ToolInput        json.RawMessage `json:"tool_input"`
	NotificationType string          `json:"notification_type"`
	Message          string          `json:"message"`
	Title            string          `json:"title"`
	LastAssistant    string          `json:"last_assistant_message"`
	Error            json.RawMessage `json:"error"`
	ErrorDetails     string          `json:"error_details"`
	Reason           string          `json:"reason"`
	Trigger          string          `json:"trigger"`
	McpServerName    string          `json:"mcp_server_name"`
}

// ParseClaude parses a Claude Code hook payload.
func ParseClaude(raw []byte) (model.Event, error) {
	var p claudePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return model.Event{}, fmt.Errorf("claude payload: %w", err)
	}
	ev := model.Event{
		Agent:          model.Claude,
		Name:           p.HookEventName,
		SessionID:      p.SessionID,
		Cwd:            p.Cwd,
		TranscriptPath: p.TranscriptPath,
		PermissionMode: p.PermissionMode,
		Model:          p.Model,
		CustomTitle:    oneLine(p.SessionTitle, maxDetail),
		SubAgent:       p.AgentID != "",
	}
	switch p.HookEventName {
	case "SessionStart":
		ev.Source = p.Source
	case "UserPromptSubmit":
		ev.Prompt = clip(stripPasted(p.Prompt), maxPrompt)
	case "PreToolUse", "PermissionRequest", "PostToolUse", "PostToolUseFailure", "PermissionDenied":
		ev.Tool = p.ToolName
		ev.ToolDetail = oneLine(ToolDetail(p.ToolName, p.ToolInput), maxDetail)
		switch {
		case p.ToolName == "AskUserQuestion":
			ev.Question = clip(askQuestionText(p.ToolInput), maxQuestion)
		case p.HookEventName == "PermissionRequest":
			q := "Allow " + p.ToolName
			if ev.ToolDetail != "" {
				q += ": " + ev.ToolDetail
			}
			ev.Question = clip(q, maxQuestion)
		}
	case "Notification":
		ev.NotificationType = p.NotificationType
		ev.Message = oneLine(p.Message, maxQuestion)
	case "Elicitation":
		msg := p.Message
		if p.McpServerName != "" {
			msg = p.McpServerName + ": " + msg
		}
		ev.Question = clip(msg, maxQuestion)
	case "Stop":
		ev.LastMessage = clip(p.LastAssistant, maxMessage)
	case "StopFailure":
		ev.Error = oneLine(strings.TrimSpace(rawString(p.Error)+" "+p.ErrorDetails), maxDetail)
		ev.LastMessage = clip(p.LastAssistant, maxMessage)
	case "PreCompact", "PostCompact":
		ev.Source = p.Trigger
	case "SessionEnd":
		ev.EndReason = p.Reason
	}
	return ev, nil
}

// agyPayload covers the fields cccp reads from Antigravity CLI hook events.
// Event names are not in the payload; the hook command passes it.
type agyPayload struct {
	ConversationID    string   `json:"conversationId"`
	WorkspacePaths    []string `json:"workspacePaths"`
	TranscriptPath    string   `json:"transcriptPath"`
	ModelName         string   `json:"modelName"`
	LastUserInput     string   `json:"lastUserInput"`
	InvocationNum     *int     `json:"invocationNum"`
	TerminationReason string   `json:"terminationReason"`
	Error             string   `json:"error"`
	ToolCall          *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"toolCall"`
}

// AgyEventEnv names the environment variable through which the agy hook
// command line passes the event name (agy payloads do not carry it).
const AgyEventEnv = "CCCP_AGY_EVENT"

// ParseAgy parses an agy hook payload. agy payloads do not carry the event
// name; the hook command line passes it in CCCP_AGY_EVENT.
func ParseAgy(raw []byte, name string) (model.Event, error) {
	var p agyPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return model.Event{}, fmt.Errorf("agy payload: %w", err)
	}
	if name == "" {
		return model.Event{}, fmt.Errorf("agy hook: %s not set", AgyEventEnv)
	}
	ev := model.Event{
		Agent:          model.Agy,
		Name:           name,
		SessionID:      p.ConversationID,
		TranscriptPath: p.TranscriptPath,
		Model:          p.ModelName,
		Prompt:         clip(stripUserRequest(p.LastUserInput), maxPrompt),
	}
	if len(p.WorkspacePaths) > 0 {
		ev.Cwd = p.WorkspacePaths[0]
	}
	switch name {
	case "PreInvocation":
		ev.FirstInvocation = p.InvocationNum != nil && *p.InvocationNum == 0
	case "PostToolUse":
		if p.ToolCall != nil {
			ev.Tool = p.ToolCall.Name
			ev.ToolDetail = oneLine(agyToolDetail(p.ToolCall.Args), maxDetail)
		}
		ev.Error = oneLine(p.Error, maxDetail)
	case "Stop":
		ev.TerminationCause = p.TerminationReason
		ev.Error = oneLine(p.Error, maxDetail)
	}
	return ev, nil
}

// ToolDetail extracts a one-line description of a Claude tool call's input.
func ToolDetail(tool string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	switch tool {
	case "Bash", "PowerShell":
		return str("command")
	case "Read", "Write", "Edit", "MultiEdit", "NotebookEdit":
		if v := str("file_path"); v != "" {
			return v
		}
		return str("notebook_path")
	case "Grep", "Glob":
		s := str("pattern")
		if p := str("path"); p != "" {
			s += "  in " + p
		}
		return s
	case "WebFetch":
		return str("url")
	case "WebSearch":
		return str("query")
	case "Agent", "Task":
		s := str("description")
		if t := str("subagent_type"); t != "" {
			s = t + ": " + s
		}
		return s
	case "AskUserQuestion":
		return askQuestionText(input)
	case "ExitPlanMode":
		return "plan ready for review"
	case "Skill":
		return str("skill")
	}
	// Generic: the first short string field, by key order, for stability.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return k + "=" + v
		}
	}
	return ""
}

func askQuestionText(input json.RawMessage) string {
	var q struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
		Question string `json:"question"`
	}
	if json.Unmarshal(input, &q) != nil {
		return ""
	}
	if q.Question != "" {
		return q.Question
	}
	var parts []string
	for _, x := range q.Questions {
		if x.Question != "" {
			parts = append(parts, x.Question)
		}
	}
	return strings.Join(parts, " / ")
}

func agyToolDetail(args json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	for _, k := range []string{"toolSummary", "CommandLine", "AbsolutePath", "TargetFile", "Query", "Url", "SearchPath"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func rawString(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(r, &s) == nil {
		return s
	}
	return string(r)
}

// stripPasted collapses <pasted_content …> blocks to a placeholder so stored
// prompts stay about what the user typed.
func stripPasted(s string) string {
	for {
		i := strings.Index(s, "<pasted_content")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "</pasted_content")
		if j < 0 {
			return s[:i] + "[pasted]"
		}
		k := strings.IndexByte(s[i+j:], '>')
		if k < 0 {
			return s[:i] + "[pasted]"
		}
		s = s[:i] + "[pasted]" + s[i+j+k+1:]
	}
}

// stripUserRequest removes agy's <USER_REQUEST> wrapper.
func stripUserRequest(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<USER_REQUEST>")
	if i := strings.Index(s, "</USER_REQUEST>"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// clip truncates s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// oneLine collapses whitespace runs (including newlines) and clips.
func oneLine(s string, n int) string {
	return clip(strings.Join(strings.Fields(s), " "), n)
}
