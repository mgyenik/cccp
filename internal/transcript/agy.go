package transcript

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

type agyEntry struct {
	off  int64
	size int64
	mod  time.Time
	meta AgyMeta
}

type agyStep struct {
	Type      string          `json:"type"`
	Status    string          `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
	Content   json.RawMessage `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls"`
	Input     int             `json:"input_tokens"`
	CacheRead int             `json:"cache_read_tokens"`
}

// Agy returns metadata for an agy transcript. agy transcripts are small, so
// they are parsed forward from the start and then incrementally.
func (r *Reader) Agy(path string) (AgyMeta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fi, err := os.Stat(path)
	if err != nil {
		delete(r.agy, path)
		return AgyMeta{}, err
	}
	e := r.agy[path]
	if e != nil && fi.Size() < e.off {
		e = nil
	}
	if e != nil && fi.Size() == e.size && fi.ModTime().Equal(e.mod) {
		return e.meta, nil
	}
	if e == nil {
		e = &agyEntry{}
		r.agy[path] = e
	}
	f, err := os.Open(path)
	if err != nil {
		return AgyMeta{}, err
	}
	defer f.Close()
	if fi.Size() > e.off {
		buf := make([]byte, fi.Size()-e.off)
		n, err := f.ReadAt(buf, e.off)
		if err != nil && err != io.EOF {
			return AgyMeta{}, err
		}
		buf = buf[:n]
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			e.parse(buf[:i+1])
			e.off += int64(i + 1)
		}
	}
	e.size, e.mod = fi.Size(), fi.ModTime()
	return e.meta, nil
}

func (e *agyEntry) parse(buf []byte) {
	for _, line := range bytes.Split(buf, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var s agyStep
		if json.Unmarshal(line, &s) != nil {
			continue
		}
		e.meta.LastStepAt = s.CreatedAt
		e.meta.LastStepType = s.Type
		e.meta.LastStepStatus = s.Status
		e.meta.PendingTools = false
		switch s.Type {
		case "USER_INPUT":
			if t := agyText(s.Content); t != "" {
				e.meta.LastPrompt = stripUserRequestTags(t)
				e.meta.LastPromptAt = s.CreatedAt
			}
		case "PLANNER_RESPONSE":
			if t := agyText(s.Content); t != "" {
				e.meta.LastMessage = t
				e.meta.LastMessageAt = s.CreatedAt
			}
			if len(s.ToolCalls) > 2 && string(s.ToolCalls) != "null" {
				e.meta.PendingTools = true
			}
			e.meta.ContextTokens = s.Input + s.CacheRead
		}
	}
}

func agyText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func stripUserRequestTags(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "<USER_REQUEST>")
	if i := strings.Index(s, "</USER_REQUEST>"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
