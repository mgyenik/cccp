package agydb

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"cccp/internal/model"
)

// Executions reads how each agy conversation's latest execution (one
// prompt's run of the agent) ended, from agy's per-conversation database
// (conversations/<id>.db, table executor_metadata), read-only.
//
// agy runs no hook when the user interrupts a turn (1.4.12 and 1.5.1,
// tested 2026-11-05, though its docs list USER_CANCELED as a Stop reason),
// and its transcript records nothing either. This table does: agy writes a
// row when an execution ends, however it ends.
type Executions struct {
	Dir   string // agy's conversations directory
	mu    sync.Mutex
	cache map[string]execEntry
}

type execEntry struct {
	stamp string
	exec  model.AgyExecution
}

// NewExecutions reads the databases in dir.
func NewExecutions(dir string) *Executions {
	return &Executions{Dir: dir, cache: map[string]execEntry{}}
}

// Last is conversation id's latest finished execution (Known false when
// there is none, or it cannot be read).
func (x *Executions) Last(id string) model.AgyExecution {
	if id == "" || filepath.Base(id) != id {
		return model.AgyExecution{}
	}
	p := filepath.Join(x.Dir, id+".db")
	st := stamp(p)
	if st == "" {
		return model.AgyExecution{}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if e, ok := x.cache[id]; ok && e.stamp == st {
		return e.exec
	}
	ex, err := lastExecution(p)
	if err != nil {
		return x.cache[id].exec // keep the last good read
	}
	x.cache[id] = execEntry{stamp: st, exec: ex}
	return ex
}

func lastExecution(p string) (model.AgyExecution, error) {
	if _, err := os.Stat(p); err != nil {
		return model.AgyExecution{}, err
	}
	dsn := "file:" + (&url.URL{Path: p}).EscapedPath() + "?mode=ro&_pragma=busy_timeout(250)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return model.AgyExecution{}, err
	}
	defer db.Close()
	var blob []byte
	err = db.QueryRow(`SELECT data FROM executor_metadata ORDER BY idx DESC LIMIT 1`).Scan(&blob)
	if err == sql.ErrNoRows {
		return model.AgyExecution{}, nil
	}
	if err != nil {
		return model.AgyExecution{}, err
	}
	return DecodeExecution(blob), nil
}

// DecodeExecution reads an executor_metadata row: a protobuf message whose
// leading varint fields are 1, the termination reason
// (ExecutorTerminationReason), 2, the number of model calls, and 3, the
// index of the execution's last step. Measured on 35 rows of two
// conversations: every normal end is 4 (NO_TOOL_CALL), every interrupt 2
// (USER_CANCELED), and field 3 equals the transcript's step index.
// Anything else (a different layout) is not Known.
func DecodeExecution(b []byte) model.AgyExecution {
	fields := map[uint64]uint64{}
	i := 0
	varint := func() (uint64, bool) {
		var x uint64
		for shift := uint(0); i < len(b) && shift < 64; shift += 7 {
			c := b[i]
			i++
			x |= uint64(c&0x7f) << shift
			if c < 0x80 {
				return x, true
			}
		}
		return 0, false
	}
	for i < len(b) && len(fields) < 3 {
		key, ok := varint()
		if !ok || key&7 != 0 { // not a varint field: the leading ones are over
			break
		}
		v, ok := varint()
		if !ok {
			return model.AgyExecution{}
		}
		fields[key>>3] = v
	}
	reason, okR := fields[1]
	last, okL := fields[3]
	if !okR || !okL || reason == 0 {
		return model.AgyExecution{}
	}
	return model.AgyExecution{Known: true, Reason: int(reason), LastStep: int(last)}
}
