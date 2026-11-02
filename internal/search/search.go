// Package search provides full-text search over agent transcripts.
//
// Only the conversation text is indexed: user prompts, assistant text and
// session titles. Tool calls and tool output are excluded; they make up most
// of the bytes and would drown real matches. Text is extracted incrementally
// (by byte offset) into a cache directory and searched with a direct scan.
// At the corpus sizes involved (hundreds of MB of raw transcripts, an order
// of magnitude less extracted) a scan takes milliseconds, so no index
// structure is maintained.
package search

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cccp/internal/model"
	"cccp/internal/paths"
	"cccp/internal/store"
)

// Doc is one transcript's extracted text.
type Doc struct {
	Agent     model.Agent `json:"agent"`
	SessionID string      `json:"session_id"`
	Source    string      `json:"source"`
	Offset    int64       `json:"offset"`
	ModTime   time.Time   `json:"mod_time"`
	Title     string      `json:"title"`
	Cwd       string      `json:"cwd"`
	Last      time.Time   `json:"last"`
}

// Key is the session key used by the engine.
func (d Doc) Key() string { return string(d.Agent) + ":" + d.SessionID }

// Hit is one matching message.
type Hit struct {
	Doc     Doc       `json:"doc"`
	Role    string    `json:"role"` // "user", "assistant" or "title"
	At      time.Time `json:"at"`
	Snippet string    `json:"snippet"`
	Matches int       `json:"matches"` // matching messages in this session
}

func textDir() string { return filepath.Join(paths.CacheDir(), "text") }

func docName(a model.Agent, id string) string {
	return store.FileName(a, id) // "<agent>-<id>.json"; the text file shares the stem
}

// Update brings the extracted text up to date with every transcript and
// returns the number of transcripts that had new content.
func Update() (int, error) {
	if err := os.MkdirAll(textDir(), 0o700); err != nil {
		return 0, err
	}
	sources := map[string]Doc{}
	claude, _ := filepath.Glob(filepath.Join(paths.ClaudeProjectsDir(), "*", "*.jsonl"))
	for _, p := range claude {
		id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		sources[docName(model.Claude, id)] = Doc{Agent: model.Claude, SessionID: id, Source: p}
	}
	agy, _ := filepath.Glob(filepath.Join(paths.AgyDir(), "brain", "*", ".system_generated", "logs", "transcript_full.jsonl"))
	for _, p := range agy {
		id := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(p))))
		sources[docName(model.Agy, id)] = Doc{Agent: model.Agy, SessionID: id, Source: p}
	}
	changed := 0
	for name, d := range sources {
		ok, err := updateOne(name, d)
		if err != nil {
			continue
		}
		if ok {
			changed++
		}
	}
	// Drop text whose transcript is gone.
	ents, _ := os.ReadDir(textDir())
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".json") {
			if _, ok := sources[n]; !ok {
				os.Remove(filepath.Join(textDir(), n))
				os.Remove(filepath.Join(textDir(), strings.TrimSuffix(n, ".json")+".txt"))
			}
		}
	}
	return changed, nil
}

func updateOne(name string, src Doc) (bool, error) {
	fi, err := os.Stat(src.Source)
	if err != nil {
		return false, err
	}
	metaPath := filepath.Join(textDir(), name)
	txtPath := strings.TrimSuffix(metaPath, ".json") + ".txt"
	var d Doc
	if b, err := os.ReadFile(metaPath); err == nil {
		_ = json.Unmarshal(b, &d)
	}
	if d.Source != src.Source || fi.Size() < d.Offset {
		d = src // new or rewritten: rebuild
		os.Remove(txtPath)
	}
	if fi.Size() == d.Offset {
		return false, nil
	}
	f, err := os.Open(src.Source)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.Seek(d.Offset, io.SeekStart); err != nil {
		return false, err
	}
	out, err := os.OpenFile(txtPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	w := bufio.NewWriter(out)
	r := bufio.NewReaderSize(f, 1<<20)
	off := d.Offset
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			off += int64(len(line))
			switch d.Agent {
			case model.Claude:
				extractClaude(&d, line, w)
			case model.Agy:
				extractAgy(&d, line, w)
			}
		}
		if err != nil {
			break // EOF; a partial last line is read next time
		}
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return false, err
	}
	out.Close()
	d.Offset, d.ModTime = off, fi.ModTime()
	b, _ := json.Marshal(d)
	return true, store.WriteAtomic(metaPath, b)
}

// Record format: one message per line: "<unix-ms>\t<role>\t<text>", with the
// text's newlines and tabs replaced by spaces.
func writeRecord(w *bufio.Writer, at time.Time, role, text string) {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return
	}
	w.WriteString(strconv.FormatInt(at.UnixMilli(), 10))
	w.WriteByte('\t')
	w.WriteString(role)
	w.WriteByte('\t')
	w.WriteString(text)
	w.WriteByte('\n')
}

type claudeLine struct {
	Type        string          `json:"type"`
	Timestamp   time.Time       `json:"timestamp"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	Cwd         string          `json:"cwd"`
	AITitle     string          `json:"aiTitle"`
	Message     json.RawMessage `json:"message"`
}

var (
	kUser      = []byte(`"type":"user"`)
	kAssistant = []byte(`"type":"assistant"`)
	kTitle     = []byte(`"type":"ai-title"`)
)

func extractClaude(d *Doc, line []byte, w *bufio.Writer) {
	if !bytes.Contains(line, kUser) && !bytes.Contains(line, kAssistant) && !bytes.Contains(line, kTitle) {
		return
	}
	var l claudeLine
	if json.Unmarshal(line, &l) != nil || l.IsSidechain {
		return
	}
	if l.Cwd != "" {
		d.Cwd = l.Cwd
	}
	if !l.Timestamp.IsZero() {
		d.Last = l.Timestamp
	}
	switch l.Type {
	case "ai-title":
		if l.AITitle != "" && l.AITitle != d.Title {
			d.Title = l.AITitle
			writeRecord(w, d.Last, "title", l.AITitle)
		}
	case "user":
		if l.IsMeta {
			return
		}
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(l.Message, &m) != nil {
			return
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			if !strings.HasPrefix(s, "<") && !strings.HasPrefix(s, "[Request interrupted") {
				writeRecord(w, l.Timestamp, "user", s)
			}
			return
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(m.Content, &blocks)
		for _, b := range blocks {
			if b.Type == "text" && !strings.HasPrefix(b.Text, "<") && !strings.HasPrefix(b.Text, "[Request interrupted") {
				writeRecord(w, l.Timestamp, "user", b.Text)
			}
		}
	case "assistant":
		var m struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(l.Message, &m) != nil {
			return
		}
		for _, b := range m.Content {
			if b.Type == "text" {
				writeRecord(w, l.Timestamp, "assistant", b.Text)
			}
		}
	}
}

func extractAgy(d *Doc, line []byte, w *bufio.Writer) {
	var s struct {
		Type      string          `json:"type"`
		CreatedAt time.Time       `json:"created_at"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(line, &s) != nil {
		return
	}
	if !s.CreatedAt.IsZero() {
		d.Last = s.CreatedAt
	}
	var text string
	if json.Unmarshal(s.Content, &text) != nil {
		return
	}
	switch s.Type {
	case "USER_INPUT":
		text = strings.TrimSpace(text)
		text = strings.TrimPrefix(text, "<USER_REQUEST>")
		if i := strings.Index(text, "</USER_REQUEST>"); i >= 0 {
			text = text[:i]
		}
		if d.Title == "" {
			d.Title = firstWords(text, 60)
		}
		writeRecord(w, s.CreatedAt, "user", text)
	case "PLANNER_RESPONSE":
		writeRecord(w, s.CreatedAt, "assistant", text)
	}
}

func firstWords(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// Query matches messages containing every term (case-insensitive).
type Query struct {
	Terms []string
}

// ParseQuery splits a query into terms; "quoted phrases" stay together.
func ParseQuery(q string) Query {
	var terms []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			terms = append(terms, strings.ToLower(t))
		}
		cur.Reset()
	}
	for _, r := range q {
		switch {
		case r == '"':
			flush()
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return Query{Terms: terms}
}

// Run searches the extracted text. Results are grouped per session: the
// most recent matching message represents its session, sessions ordered by
// that message's time, newest first.
func Run(q Query, limit int) ([]Hit, error) {
	if len(q.Terms) == 0 {
		return nil, errors.New("empty query")
	}
	ents, err := os.ReadDir(textDir())
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		metaPath := filepath.Join(textDir(), e.Name())
		var d Doc
		b, err := os.ReadFile(metaPath)
		if err != nil || json.Unmarshal(b, &d) != nil {
			continue
		}
		h, ok := scanDoc(d, strings.TrimSuffix(metaPath, ".json")+".txt", q)
		if ok {
			hits = append(hits, h)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].At.After(hits[j].At) })
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func scanDoc(d Doc, txt string, q Query) (Hit, bool) {
	f, err := os.Open(txt)
	if err != nil {
		return Hit{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var best Hit
	found := false
	n := 0
	for sc.Scan() {
		line := sc.Bytes()
		low := bytes.ToLower(line)
		all := true
		for _, t := range q.Terms {
			if !bytes.Contains(low, []byte(t)) {
				all = false
				break
			}
		}
		if !all {
			continue
		}
		n++
		parts := strings.SplitN(string(line), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		ms, _ := strconv.ParseInt(parts[0], 10, 64)
		at := time.UnixMilli(ms)
		if !found || !at.Before(best.At) {
			best = Hit{Doc: d, Role: parts[1], At: at, Snippet: snippet(parts[2], q.Terms[0], 160)}
			found = true
		}
	}
	best.Matches = n
	return best, found
}

// snippet returns about width characters of text centred on the first
// occurrence of term.
func snippet(text, term string, width int) string {
	low := strings.ToLower(text)
	i := strings.Index(low, term)
	if i < 0 {
		i = 0
	}
	start := i - width/3
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(text) {
		end = len(text)
	}
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	s := text[start:end]
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return s
}
