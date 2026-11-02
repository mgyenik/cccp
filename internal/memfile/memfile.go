// Package memfile persists the daemon's memory as a JSON file.
package memfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"cccp/internal/core"
	"cccp/internal/store"
)

// File is the memory file at Path.
type File struct{ Path string }

// Load reads memory; a missing file yields empty memory. A corrupt file is
// reported and replaced by empty memory (the caller logs the error and
// carries on: losing seen marks is better than not starting).
func (f File) Load() (core.Memory, error) {
	m := core.NewMemory()
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	var loaded core.Memory
	if err := json.Unmarshal(b, &loaded); err != nil {
		return m, fmt.Errorf("%s: %w", f.Path, err)
	}
	return loaded.Clone(), nil // Clone fills nil maps
}

// Save writes memory atomically.
func (f File) Save(m core.Memory) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(f.Path, b)
}
