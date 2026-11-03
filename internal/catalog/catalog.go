// Package catalog indexes every agent conversation on disk: per transcript,
// the facts shown in the session list (title, working directory, branch,
// model, first and last prompt, last message, turn count, activity times),
// and the conversation text for full-text search.
//
// Only the conversation text is indexed: user prompts, assistant text and
// session titles. Tool calls and tool output are excluded; they make up most
// of the bytes and would drown real matches. Text is extracted incrementally
// (by byte offset) into a cache directory and searched with a direct scan.
// At the corpus sizes involved (hundreds of MB of raw transcripts, an order
// of magnitude less extracted) a scan takes milliseconds, so no index
// structure is maintained.
package catalog

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

	"golang.org/x/sys/unix"

	"cccp/internal/model"
	"cccp/internal/store"
)

// Version of the cached Doc format; a Doc with another version is rebuilt.
const Version = 2

// Doc is one conversation: its catalog facts and the state of its
// incremental text extraction.
type Doc struct {
	Version   int         `json:"version"`
	Agent     model.Agent `json:"agent"`
	SessionID string      `json:"session_id"`
	// ConfigDir is the Claude profile the transcript belongs to.
	ConfigDir string    `json:"config_dir,omitempty"`
	Source    string    `json:"source"`
	Offset    int64     `json:"offset"`
	ModTime   time.Time `json:"mod_time"`

	Title       string    `json:"title"` // Claude's ai-title; agy's first prompt
	Cwd         string    `json:"cwd"`
	Branch      string    `json:"branch,omitempty"`
	Model       string    `json:"model,omitempty"`
	Entrypoint  string    `json:"entrypoint,omitempty"` // "cli"; "sdk-cli" for print mode
	FirstPrompt string    `json:"first_prompt,omitempty"`
	LastPrompt  string    `json:"last_prompt,omitempty"`
	LastMessage string    `json:"last_message,omitempty"` // the end of the last assistant text
	Turns       int       `json:"turns"`                  // prompts the user typed
	First       time.Time `json:"first"`
	Last        time.Time `json:"last"`
}

// PrintMode reports whether the conversation was a `claude -p` run.
func (d Doc) PrintMode() bool { return strings.HasPrefix(d.Entrypoint, "sdk") }

// Text limits for the stored facts, in bytes.
const (
	maxPrompt  = 1000
	maxMessage = 2000
)

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

// Index is the extracted-text cache and the transcript directories it is
// built from.
type Index struct {
	TextDir string
	// ClaudeConfigDirs are the Claude profiles whose transcripts are indexed.
	ClaudeConfigDirs []string
	AgyBrainDir      string
}

func docName(a model.Agent, id string) string {
	return store.FileName(a, id) // "<agent>-<id>.json"; the text file shares the stem
}

// Update brings the catalog up to date with every transcript and returns
// the number of transcripts that had new content. It holds an exclusive
// lock on the cache: the daemon and an in-process cockpit may both update.
func (ix Index) Update() (int, error) {
	if err := os.MkdirAll(ix.TextDir, 0o700); err != nil {
		return 0, err
	}
	lf, err := os.OpenFile(filepath.Join(ix.TextDir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, err
	}
	defer lf.Close()
	if err := unix.Flock(int(lf.Fd()), unix.LOCK_EX); err != nil {
		return 0, err
	}
	defer unix.Flock(int(lf.Fd()), unix.LOCK_UN)
	sources := map[string]Doc{}
	for _, cfg := range ix.ClaudeConfigDirs {
		claude, _ := filepath.Glob(filepath.Join(cfg, "projects", "*", "*.jsonl"))
		for _, p := range claude {
			id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
			sources[docName(model.Claude, id)] = Doc{Agent: model.Claude, SessionID: id, Source: p, ConfigDir: cfg}
		}
	}
	agy, _ := filepath.Glob(filepath.Join(ix.AgyBrainDir, "*", ".system_generated", "logs", "transcript_full.jsonl"))
	for _, p := range agy {
		id := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(p))))
		sources[docName(model.Agy, id)] = Doc{Agent: model.Agy, SessionID: id, Source: p}
	}
	changed := 0
	for name, d := range sources {
		ok, err := ix.updateOne(name, d)
		if err != nil {
			continue
		}
		if ok {
			changed++
		}
	}
	// Drop text whose transcript is gone.
	ents, _ := os.ReadDir(ix.TextDir)
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") {
			if _, ok := sources[n]; !ok {
				os.Remove(filepath.Join(ix.TextDir, n))
				os.Remove(filepath.Join(ix.TextDir, strings.TrimSuffix(n, ".json")+".txt"))
			}
		}
	}
	return changed, nil
}

func (ix Index) updateOne(name string, src Doc) (bool, error) {
	fi, err := os.Stat(src.Source)
	if err != nil {
		return false, err
	}
	metaPath := filepath.Join(ix.TextDir, name)
	txtPath := strings.TrimSuffix(metaPath, ".json") + ".txt"
	var d Doc
	if b, err := os.ReadFile(metaPath); err == nil {
		_ = json.Unmarshal(b, &d)
	}
	if d.Version != Version || d.Source != src.Source || fi.Size() < d.Offset {
		d = src // new, rewritten or an older format: rebuild
		d.Version = Version
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
	GitBranch   string          `json:"gitBranch"`
	Entrypoint  string          `json:"entrypoint"`
	AITitle     string          `json:"aiTitle"`
	Message     json.RawMessage `json:"message"`
}

// prompt records a prompt the user typed.
func (d *Doc) prompt(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if d.FirstPrompt == "" {
		d.FirstPrompt = clip(text, maxPrompt)
	}
	d.LastPrompt = clip(text, maxPrompt)
	d.Turns++
}

// clip keeps at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// tailText keeps at most the last n bytes on a rune boundary.
func tailText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "…" + s[cut:]
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
	if l.GitBranch != "" {
		d.Branch = l.GitBranch
	}
	if l.Entrypoint != "" && d.Entrypoint == "" {
		d.Entrypoint = l.Entrypoint
	}
	if !l.Timestamp.IsZero() {
		if d.First.IsZero() {
			d.First = l.Timestamp
		}
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
				d.prompt(s)
			}
			return
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(m.Content, &blocks)
		var typed []string
		for _, b := range blocks {
			if b.Type == "text" && !strings.HasPrefix(b.Text, "<") && !strings.HasPrefix(b.Text, "[Request interrupted") {
				writeRecord(w, l.Timestamp, "user", b.Text)
				typed = append(typed, b.Text)
			}
		}
		if len(typed) > 0 {
			d.prompt(strings.Join(typed, "\n"))
		}
	case "assistant":
		var m struct {
			Model   string `json:"model"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(l.Message, &m) != nil {
			return
		}
		if m.Model != "" && m.Model != "<synthetic>" {
			d.Model = m.Model
		}
		for _, b := range m.Content {
			if b.Type == "text" {
				writeRecord(w, l.Timestamp, "assistant", b.Text)
				if strings.TrimSpace(b.Text) != "" {
					d.LastMessage = tailText(strings.TrimSpace(b.Text), maxMessage)
				}
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
		if d.First.IsZero() {
			d.First = s.CreatedAt
		}
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
		d.prompt(text)
	case "PLANNER_RESPONSE":
		writeRecord(w, s.CreatedAt, "assistant", text)
		if strings.TrimSpace(text) != "" {
			d.LastMessage = tailText(strings.TrimSpace(text), maxMessage)
		}
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

// Docs returns every cataloged conversation (print-mode runs excluded),
// newest first. Call Update first to bring the catalog up to date.
func (ix Index) Docs() ([]Doc, error) {
	ents, err := os.ReadDir(ix.TextDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Doc
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".json") || strings.HasPrefix(n, ".") {
			continue
		}
		var d Doc
		b, err := os.ReadFile(filepath.Join(ix.TextDir, n))
		if err != nil || json.Unmarshal(b, &d) != nil || d.Version != Version || d.PrintMode() {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].Key() < out[j].Key()
	})
	return out, nil
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
func (ix Index) Run(q Query, limit int) ([]Hit, error) {
	if len(q.Terms) == 0 {
		return nil, errors.New("empty query")
	}
	ents, err := os.ReadDir(ix.TextDir)
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		metaPath := filepath.Join(ix.TextDir, e.Name())
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
