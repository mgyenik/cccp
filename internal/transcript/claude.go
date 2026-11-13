// Package transcript extracts slow-moving facts from agent transcripts:
// titles, last prompt/response, cache TTL bucket and last activity, context
// size, and (for sessions without hooks) an inferred activity.
//
// Transcripts can be tens of megabytes. The first read scans backwards from
// the end in growing windows until every wanted fact is found; after that
// only appended bytes are parsed.
package transcript

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"cccp/internal/model"
)

// The metadata types are plain data, in model so the functional core can use
// them.
type (
	ClaudeMeta = model.ClaudeMeta
	AgyMeta    = model.AgyMeta
)

// TTL buckets of the prompt cache.
const (
	TTL5m = model.TTL5m
	TTL1h = model.TTL1h
)

type claudeEntry struct {
	off  int64 // scanned up to (exclusive), always at a line boundary
	size int64
	mod  time.Time
	meta ClaudeMeta
	// sequence numbers, so facts found by the backward scan never override
	// later facts from forward reads
	seqTitle, seqPrompt, seqAssist, seqReq, seqTTL, seqUser int64
}

// Reader caches transcript metadata by path.
type Reader struct {
	mu    sync.Mutex
	cache map[string]*claudeEntry
	agy   map[string]*agyEntry
}

// NewReader returns an empty cache.
func NewReader() *Reader {
	return &Reader{cache: map[string]*claudeEntry{}, agy: map[string]*agyEntry{}}
}

// Backward-scan window sizes (variables so tests can shrink them).
var (
	initialWindow int64 = 2 << 20
	maxWindow     int64 = 256 << 20
)

// Claude returns metadata for a Claude transcript, reading only what changed
// since the last call.
func (r *Reader) Claude(path string) (ClaudeMeta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fi, err := os.Stat(path)
	if err != nil {
		delete(r.cache, path)
		return ClaudeMeta{}, err
	}
	e := r.cache[path]
	if e != nil && fi.Size() < e.off {
		e = nil // rewritten or truncated
	}
	if e != nil && fi.Size() == e.size && fi.ModTime().Equal(e.mod) {
		return e.meta, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return ClaudeMeta{}, err
	}
	defer f.Close()
	if e == nil {
		e = &claudeEntry{}
		if err := e.backward(f, fi.Size()); err != nil {
			return ClaudeMeta{}, err
		}
		r.cache[path] = e
	} else if err := e.forward(f, fi.Size()); err != nil {
		return ClaudeMeta{}, err
	}
	e.size, e.mod = fi.Size(), fi.ModTime()
	return e.meta, nil
}

// backward scans growing windows from the end until all facts are known.
func (e *claudeEntry) backward(f *os.File, size int64) error {
	end := lastNewline(f, size)
	window := initialWindow
	for {
		start := end - window
		if start < 0 {
			start = 0
		}
		buf := make([]byte, end-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return err
		}
		if start > 0 {
			// drop the partial first line
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			} else {
				buf = nil
			}
		}
		// Re-parse the window from scratch: sequence numbers are line
		// positions within it, so later lines always win.
		*e = claudeEntry{}
		e.parse(buf, 0)
		if start == 0 || window >= maxWindow || e.complete() {
			break
		}
		window *= 4
	}
	e.off = end
	return nil
}

func (e *claudeEntry) complete() bool {
	return e.meta.AITitle != "" && e.meta.TTL != 0 && e.meta.LastPrompt != "" && !e.meta.LastAssistantAt.IsZero()
}

// forward parses complete lines appended since the last read.
func (e *claudeEntry) forward(f *os.File, size int64) error {
	if size <= e.off {
		return nil
	}
	buf := make([]byte, size-e.off)
	n, err := f.ReadAt(buf, e.off)
	if err != nil && err != io.EOF {
		return err
	}
	buf = buf[:n]
	i := bytes.LastIndexByte(buf, '\n')
	if i < 0 {
		return nil // no complete line yet
	}
	e.parse(buf[:i+1], e.off)
	e.off += int64(i + 1)
	return nil
}

// lastNewline returns the offset just past the last '\n' in the file, so
// a partially written final line is left for a later forward read.
func lastNewline(f *os.File, size int64) int64 {
	const chunk = 64 << 10
	for end := size; end > 0; {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		buf := make([]byte, end-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return 0
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			return start + int64(i) + 1
		}
		end = start
	}
	return 0
}

var (
	kAssistant  = []byte(`"type":"assistant"`)
	kUser       = []byte(`"type":"user"`)
	kTitle      = []byte(`"type":"ai-title"`)
	kLastPrompt = []byte(`"type":"last-prompt"`)
	kAPIReq1    = []byte(`"type":"api-request",`)
	kAPIReq2    = []byte(`"type":"api-request"}`)
)

type rawEntry struct {
	Type        string    `json:"type"`
	SessionID   string    `json:"sessionId"`
	Timestamp   time.Time `json:"timestamp"`
	IsSidechain bool      `json:"isSidechain"`
	IsMeta      bool      `json:"isMeta"`
	AITitle     string    `json:"aiTitle"`
	LastPrompt  string    `json:"lastPrompt"`
	QuerySource string    `json:"querySource"`
	// Who started a turn: turnOrigin "human", "task_notification",
	// "peer"…; promptSource "typed", "suggestion_accepted", "system"….
	TurnOrigin   string          `json:"turnOrigin"`
	PromptSource string          `json:"promptSource"`
	Message      json.RawMessage `json:"message"`
}

type rawMessage struct {
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Content    json.RawMessage `json:"content"`
	Usage      *struct {
		Input         int `json:"input_tokens"`
		CacheRead     int `json:"cache_read_input_tokens"`
		CacheCreation int `json:"cache_creation_input_tokens"`
		Split         *struct {
			M5 int `json:"ephemeral_5m_input_tokens"`
			H1 int `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	} `json:"usage"`
}

type rawBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

func (e *claudeEntry) parse(buf []byte, base int64) {
	pos := base
	for len(buf) > 0 {
		var line []byte
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			line, buf = buf[:i], buf[i+1:]
		} else {
			line, buf = buf, nil
		}
		pos += int64(len(line)) + 1
		seq := pos
		switch {
		case bytes.Contains(line, kTitle):
			var r rawEntry
			if json.Unmarshal(line, &r) == nil && r.AITitle != "" && seq > e.seqTitle {
				e.meta.AITitle, e.seqTitle = r.AITitle, seq
				e.setSession(r.SessionID)
			}
		case bytes.Contains(line, kLastPrompt):
			var r rawEntry
			if json.Unmarshal(line, &r) == nil && r.LastPrompt != "" && seq > e.seqPrompt {
				e.meta.LastPrompt, e.seqPrompt = r.LastPrompt, seq
			}
		case bytes.Contains(line, kAPIReq1) || bytes.Contains(line, kAPIReq2):
			var r rawEntry
			if json.Unmarshal(line, &r) == nil && r.QuerySource == "repl_main_thread" && seq > e.seqReq {
				e.meta.LastRequestAt, e.seqReq = r.Timestamp, seq
			}
		case bytes.Contains(line, kAssistant):
			e.parseAssistant(line, seq)
		case bytes.Contains(line, kUser):
			e.parseUser(line, seq)
		}
	}
}

func (e *claudeEntry) setSession(id string) {
	if id != "" && e.meta.SessionID == "" {
		e.meta.SessionID = id
	}
}

func (e *claudeEntry) parseAssistant(line []byte, seq int64) {
	var r rawEntry
	if json.Unmarshal(line, &r) != nil || r.Type != "assistant" || r.IsSidechain {
		return
	}
	var m rawMessage
	if json.Unmarshal(r.Message, &m) != nil || m.Model == "<synthetic>" {
		return
	}
	e.setSession(r.SessionID)
	if m.Usage != nil && m.Usage.Split != nil && seq > e.seqTTL {
		switch {
		case m.Usage.Split.H1 > 0:
			e.meta.TTL, e.seqTTL = TTL1h, seq
		case m.Usage.Split.M5 > 0:
			e.meta.TTL, e.seqTTL = TTL5m, seq
		}
	}
	if seq < e.seqAssist {
		return
	}
	e.seqAssist = seq
	e.meta.LastAssistantAt = r.Timestamp
	e.meta.LastStopReason = m.StopReason
	if m.Model != "" {
		e.meta.Model = m.Model
	}
	if m.Usage != nil {
		e.meta.ContextTokens = m.Usage.Input + m.Usage.CacheRead + m.Usage.CacheCreation
	}
	var blocks []rawBlock
	_ = json.Unmarshal(m.Content, &blocks)
	var text []string
	tool := ""
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				text = append(text, b.Text)
			}
		case "tool_use":
			tool = b.Name
		}
	}
	if len(text) > 0 {
		e.meta.LastAssistantText = strings.Join(text, "\n")
	}
	// Claude Code writes one transcript entry per content block, so the
	// tool_use block and its stop_reason can be on different entries.
	if tool != "" || m.StopReason == "tool_use" {
		if seq > e.seqUser {
			e.meta.PendingTool = true
			if tool != "" {
				e.meta.PendingToolName = tool
			}
		}
	} else if m.StopReason != "" {
		e.meta.PendingTool, e.meta.PendingToolName = false, ""
	}
	e.meta.Interrupted = false
}

// turnStart reports whether a user entry starts a turn, and whether Claude
// Code started it on its own (not the user). Entries record it in
// turnOrigin or promptSource; older ones carry neither, and then a task
// notification is recognised by its text and any other plain prompt is the
// user's (meta entries and command wrappers start no turn).
func turnStart(r rawEntry, prompt string) (starts, auto bool) {
	switch {
	case r.TurnOrigin != "":
		return true, r.TurnOrigin != "human"
	case r.PromptSource != "":
		return true, r.PromptSource == "system"
	case model.AutoPrompt(prompt):
		return true, true
	case r.IsMeta || prompt == "" || strings.HasPrefix(prompt, "<") || strings.HasPrefix(prompt, "[Request interrupted by user"):
		return false, false
	}
	return true, false
}

func (e *claudeEntry) parseUser(line []byte, seq int64) {
	var r rawEntry
	if json.Unmarshal(line, &r) != nil || r.Type != "user" || r.IsSidechain {
		return
	}
	var m rawMessage
	if json.Unmarshal(r.Message, &m) != nil {
		return
	}
	e.setSession(r.SessionID)
	if seq < e.seqUser {
		return
	}
	var prompt string
	toolResult := false
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		prompt = s
	} else {
		var blocks []rawBlock
		_ = json.Unmarshal(m.Content, &blocks)
		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				toolResult = true
			case "text":
				if prompt == "" {
					prompt = b.Text
				}
			}
		}
	}
	e.seqUser = seq
	e.meta.LastUserAt = r.Timestamp
	if !toolResult {
		if starts, auto := turnStart(r, prompt); starts {
			e.meta.LastTurnAuto, e.meta.LastTurnAt = auto, r.Timestamp
			if !auto && !r.IsMeta && !strings.HasPrefix(prompt, "<") {
				e.meta.RecentPrompts = model.AddPrompt(e.meta.RecentPrompts, prompt, r.Timestamp)
			}
		}
	}
	if toolResult {
		e.meta.PendingTool, e.meta.PendingToolName = false, ""
		e.meta.Interrupted = false
		return
	}
	if strings.HasPrefix(prompt, "[Request interrupted by user") {
		e.meta.Interrupted = true
		e.meta.PendingTool, e.meta.PendingToolName = false, ""
		return
	}
	if r.IsMeta || prompt == "" || strings.HasPrefix(prompt, "<") {
		// command wrappers, caveats, system reminders
		return
	}
	e.meta.Interrupted = false
	e.meta.LastPromptAt = r.Timestamp
	if e.seqPrompt == 0 {
		// Only when no "last-prompt" entry has been seen: those are the
		// authoritative record of what the user typed.
		e.meta.LastPrompt = prompt
	}
}
