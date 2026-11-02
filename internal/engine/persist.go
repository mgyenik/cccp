package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"cccp/internal/paths"
	"cccp/internal/store"
)

// SummaryRecord is a generated one-line summary and the input it was made from.
type SummaryRecord struct {
	InputHash string    `json:"input_hash"`
	Text      string    `json:"text"`
	At        time.Time `json:"at"`
}

// Persist is the daemon-owned state that must survive restarts. Hooks never
// write it.
type Persist struct {
	// Seen: last time each session's pane was visible on a focused client.
	Seen map[string]time.Time `json:"seen"`
	// Aliases set by the user in the cockpit.
	Aliases map[string]string `json:"aliases"`
	// RegCompletions: turn ends observed as registry busy→idle transitions
	// (for sessions without hooks).
	RegCompletions map[string]time.Time     `json:"reg_completions"`
	Summaries      map[string]SummaryRecord `json:"summaries"`
	// Initialized is set once the first discovery has marked all
	// pre-existing sessions as seen.
	Initialized bool `json:"initialized"`
}

func newPersist() *Persist {
	return &Persist{
		Seen:           map[string]time.Time{},
		Aliases:        map[string]string{},
		RegCompletions: map[string]time.Time{},
		Summaries:      map[string]SummaryRecord{},
	}
}

// LoadPersist reads daemon.json; a missing file yields empty state.
func LoadPersist() (*Persist, error) {
	p := newPersist()
	b, err := os.ReadFile(paths.DaemonStateFile())
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, p); err != nil {
		return newPersist(), err
	}
	if p.Seen == nil {
		p.Seen = map[string]time.Time{}
	}
	if p.Aliases == nil {
		p.Aliases = map[string]string{}
	}
	if p.RegCompletions == nil {
		p.RegCompletions = map[string]time.Time{}
	}
	if p.Summaries == nil {
		p.Summaries = map[string]SummaryRecord{}
	}
	return p, nil
}

// Save writes daemon.json atomically.
func (p *Persist) Save() error {
	if err := os.MkdirAll(filepath.Dir(paths.DaemonStateFile()), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", " ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(paths.DaemonStateFile(), b)
}

// Prune drops records for keys not in keep.
func (p *Persist) Prune(keep map[string]bool) {
	for k := range p.Seen {
		if !keep[k] {
			delete(p.Seen, k)
		}
	}
	for k := range p.RegCompletions {
		if !keep[k] {
			delete(p.RegCompletions, k)
		}
	}
	for k := range p.Summaries {
		if !keep[k] {
			delete(p.Summaries, k)
		}
	}
	// Aliases are user data: kept even for sessions that are gone, so a
	// resumed session gets its alias back.
}
