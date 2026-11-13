package core

import (
	"maps"
	"time"

	"cccp/internal/jail"
	"cccp/internal/workstream"
)

// SummaryRecord is a generated one-line summary and the input it was made from.
type SummaryRecord struct {
	InputHash string    `json:"input_hash"`
	Text      string    `json:"text"`
	At        time.Time `json:"at"`
	// Verdict is a live session's VerdictAct or VerdictWait ("" for a closed
	// conversation, or a summary that gave none).
	Verdict string `json:"verdict,omitempty"`
}

// TitleRecord is a working title.
type TitleRecord struct {
	Text string `json:"text"`
	// BasedOn is the newest prompt it was made from: prompts after it are
	// new to it.
	BasedOn time.Time `json:"based_on"`
	At      time.Time `json:"at"`
}

// RegStatus is a registry session's last observed status.
type RegStatus struct {
	Status string
	At     time.Time
}

// Memory is what cccp remembers between observations. It is a value:
// Compute returns a new Memory and never modifies the one it was given.
type Memory struct {
	// Seen: when each session's latest completion was looked at (its pane
	// visible on a focused client), or when the session was first seen.
	Seen map[string]time.Time `json:"seen"`
	// Titles are cccp's working titles: what each conversation is about now,
	// from its recent prompts (Claude Code and agy title a conversation once,
	// from its first prompts).
	Titles map[string]TitleRecord `json:"titles,omitempty"`
	// Aliases set by the user.
	Aliases map[string]string `json:"aliases"`
	// RegCompletions: turn ends observed as registry busy/waiting → idle
	// transitions (sessions without hooks).
	RegCompletions map[string]time.Time `json:"reg_completions"`
	// Summaries are persisted separately (memfile): they cost a model call
	// each and grow with the number of conversations.
	Summaries map[string]SummaryRecord `json:"-"`
	// Initialized is set once a first observation with sessions has been
	// computed; until then every session is treated as already seen.
	Initialized bool `json:"initialized"`
	// RegLast is each registry session's previous status, for transition
	// detection. Not persisted: after a restart there is no previous status.
	RegLast map[string]RegStatus `json:"-"`
	// Workstreams is the record of tmux sessions, for restoring them
	// (persisted separately, memfile).
	Workstreams workstream.Record `json:"-"`
	// Boosts are workstreams' temporary limits (docs/jails.md), by name.
	Boosts map[string]jail.Boost `json:"boosts,omitempty"`
}

// NewMemory returns empty memory.
func NewMemory() Memory {
	return Memory{
		Seen:           map[string]time.Time{},
		Aliases:        map[string]string{},
		RegCompletions: map[string]time.Time{},
		Summaries:      map[string]SummaryRecord{},
		Titles:         map[string]TitleRecord{},
		RegLast:        map[string]RegStatus{},
		Boosts:         map[string]jail.Boost{},
	}
}

// Clone returns a deep copy (nil maps become empty).
func (m Memory) Clone() Memory {
	c := NewMemory()
	maps.Copy(c.Seen, m.Seen)
	maps.Copy(c.Aliases, m.Aliases)
	maps.Copy(c.RegCompletions, m.RegCompletions)
	maps.Copy(c.Summaries, m.Summaries)
	maps.Copy(c.Titles, m.Titles)
	maps.Copy(c.RegLast, m.RegLast)
	c.Initialized = m.Initialized
	c.Workstreams = m.Workstreams.Clone()
	maps.Copy(c.Boosts, m.Boosts)
	return c
}

// Persisted reports whether the persisted parts of m and o are equal.
func (m Memory) Persisted(o Memory) bool {
	return m.Initialized == o.Initialized &&
		maps.EqualFunc(m.Seen, o.Seen, time.Time.Equal) &&
		maps.Equal(m.Aliases, o.Aliases) &&
		maps.EqualFunc(m.RegCompletions, o.RegCompletions, time.Time.Equal) &&
		maps.EqualFunc(m.Titles, o.Titles, func(a, b TitleRecord) bool {
			return a.Text == b.Text && a.BasedOn.Equal(b.BasedOn) && a.At.Equal(b.At)
		}) &&
		maps.EqualFunc(m.Summaries, o.Summaries, func(a, b SummaryRecord) bool {
			return a.InputHash == b.InputHash && a.Text == b.Text && a.At.Equal(b.At) && a.Verdict == b.Verdict
		}) &&
		m.Workstreams.Equal(o.Workstreams) &&
		maps.EqualFunc(m.Boosts, o.Boosts, func(a, b jail.Boost) bool {
			return a.Preset == b.Preset && a.Until.Equal(b.Until)
		})
}

// Prune returns a copy without records for sessions not in keep. Aliases
// are user data and are kept, so a resumed session gets its alias back.
func (m Memory) Prune(keep map[string]bool) Memory {
	c := m.Clone()
	for _, mm := range []map[string]time.Time{c.Seen, c.RegCompletions} {
		for k := range mm {
			if !keep[k] {
				delete(mm, k)
			}
		}
	}
	for k := range c.Summaries {
		if !keep[k] {
			delete(c.Summaries, k)
		}
	}
	for k := range c.Titles {
		if !keep[k] {
			delete(c.Titles, k)
		}
	}
	for k := range c.RegLast {
		if !keep[k] {
			delete(c.RegLast, k)
		}
	}
	return c
}
