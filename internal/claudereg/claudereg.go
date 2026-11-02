// Package claudereg reads Claude Code's live session registry,
// ~/.claude/sessions/<pid>.json.
//
// The registry is not a documented-stable format (the documented interface is
// `claude agents --json`, which is too heavy to poll), so parsing is lenient:
// unknown fields are ignored and unknown status values map to Unknown.
package claudereg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"cccp/internal/model"
)

// Entry is one registry file (plain data, in model so the functional core
// can use it).
type Entry = model.RegistryEntry

// Read parses one registry file.
func Read(p string) (Entry, error) {
	var e Entry
	b, err := os.ReadFile(p)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, err
	}
	e.Path = p
	return e, nil
}

// IsEntryFile reports whether a file name in the registry dir is an entry.
func IsEntryFile(name string) bool {
	if !strings.HasSuffix(name, ".json") {
		return false
	}
	_, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
	return err == nil
}

// ReadAll parses every entry in the registry directory dir.
func ReadAll(dir string) []Entry {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Entry
	for _, d := range ents {
		if !IsEntryFile(d.Name()) {
			continue
		}
		if e, err := Read(filepath.Join(dir, d.Name())); err == nil {
			out = append(out, e)
		}
	}
	return out
}

// Dir is the registry directory, as a source for the gatherer.
type Dir struct{ Path string }

// ReadAll parses every entry in the directory.
func (d Dir) ReadAll() []Entry { return ReadAll(d.Path) }

// Read parses one entry.
func (Dir) Read(path string) (Entry, error) { return Read(path) }
