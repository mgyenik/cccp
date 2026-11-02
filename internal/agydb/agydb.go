// Package agydb reads agy's conversation summary database (read-only).
package agydb

import (
	"database/sql"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"cccp/internal/model"
)

// Conversation is one row of agy's conversation summaries (plain data, in
// model so the functional core can use it).
type Conversation = model.AgyConversation

// Cache re-reads the database at Path only when it (or its WAL) changed.
type Cache struct {
	Path  string
	mu    sync.Mutex
	stamp string
	rows  map[string]Conversation
}

// NewCache returns a cache for the database at path.
func NewCache(path string) *Cache { return &Cache{Path: path} }

func stamp(p string) string {
	var b strings.Builder
	for _, f := range []string{p, p + "-wal"} {
		if fi, err := os.Stat(f); err == nil {
			b.WriteString(fi.ModTime().String())
			b.WriteString(":")
		}
	}
	return b.String()
}

// All returns every conversation, keyed by id.
func (c *Cache) All() map[string]Conversation {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.Path
	st := stamp(p)
	if st == "" {
		return nil
	}
	if st == c.stamp && c.rows != nil {
		return c.rows
	}
	rows, err := read(p)
	if err != nil {
		return c.rows // keep the last good read
	}
	c.stamp, c.rows = st, rows
	return rows
}

func read(p string) (map[string]Conversation, error) {
	// mode=ro: never write. The database is in WAL mode and agy holds it
	// open; read-only connections can still read the WAL.
	dsn := "file:" + (&url.URL{Path: p}).EscapedPath() + "?mode=ro&_pragma=busy_timeout(250)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	q, err := db.Query(`SELECT conversation_id, title, preview, workspace_uris, last_modified_time FROM conversation_summaries`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	out := map[string]Conversation{}
	for q.Next() {
		var id, title, preview, ws string
		var mod any
		if err := q.Scan(&id, &title, &preview, &ws, &mod); err != nil {
			continue
		}
		c := Conversation{ID: id, Title: title, Preview: preview, Workspaces: parseWorkspaces(ws)}
		c.LastModified = parseTime(mod)
		out[id] = c
	}
	return out, q.Err()
}

// parseWorkspaces parses the JSON-ish list of file:// URIs.
func parseWorkspaces(s string) []string {
	s = strings.Trim(strings.TrimSpace(s), "[]")
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.Trim(strings.TrimSpace(part), `"`)
		if u, err := url.Parse(part); err == nil && u.Scheme == "file" {
			out = append(out, u.Path)
		}
	}
	return out
}

func parseTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		for _, layout := range []string{"2006-01-02 15:04:05.999999999-07:00", time.RFC3339Nano} {
			if x, err := time.Parse(layout, t); err == nil {
				return x
			}
		}
	}
	return time.Time{}
}
