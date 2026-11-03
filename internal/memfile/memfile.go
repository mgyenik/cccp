// Package memfile persists the daemon's memory: user state in one JSON file
// (seen marks, aliases, registry completions), and summaries in another.
// Summaries are kept apart because each cost a model call to make and their
// number grows with the number of conversations.
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

// File is the memory at Path, with summaries at SummariesPath.
type File struct {
	Path          string
	SummariesPath string
}

// legacy is the part of an older daemon.json that held summaries.
type legacy struct {
	Summaries map[string]core.SummaryRecord `json:"summaries"`
}

// Load reads memory; missing files yield empty memory. A corrupt file is
// reported and the rest still loaded: losing seen marks or summaries is
// better than not starting.
func (f File) Load() (core.Memory, error) {
	m := core.NewMemory()
	var errs []error
	if b, err := os.ReadFile(f.Path); err == nil {
		var loaded core.Memory
		if err := json.Unmarshal(b, &loaded); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.Path, err))
		} else {
			m = loaded.Clone() // Clone fills nil maps
		}
		var old legacy
		if json.Unmarshal(b, &old) == nil {
			for k, v := range old.Summaries {
				m.Summaries[k] = v // migrated; summaries.json wins below
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if b, err := os.ReadFile(f.SummariesPath); err == nil {
		var sums map[string]core.SummaryRecord
		if err := json.Unmarshal(b, &sums); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.SummariesPath, err))
		}
		for k, v := range sums {
			m.Summaries[k] = v
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	return m, errors.Join(errs...)
}

// Save writes both files atomically.
func (f File) Save(m core.Memory) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	if err := store.WriteAtomic(f.Path, b); err != nil {
		return err
	}
	b, err = json.MarshalIndent(m.Summaries, "", " ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(f.SummariesPath, b)
}
