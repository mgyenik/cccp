// Package procs reads process information from /proc.
package procs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Stat is the subset of /proc/<pid>/stat cccp uses.
type Stat struct {
	PID       int
	Comm      string
	State     byte
	PPID      int
	StartTime uint64 // field 22, clock ticks since boot
}

// ReadStat parses /proc/<pid>/stat. The comm field may contain spaces and
// parentheses, so it is delimited by the first '(' and the last ')'.
func ReadStat(pid int) (Stat, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Stat{}, err
	}
	return parseStat(pid, b)
}

func parseStat(pid int, b []byte) (Stat, error) {
	l := bytes.IndexByte(b, '(')
	r := bytes.LastIndexByte(b, ')')
	if l < 0 || r < l {
		return Stat{}, errors.New("malformed stat")
	}
	st := Stat{PID: pid, Comm: string(b[l+1 : r])}
	f := strings.Fields(string(b[r+1:]))
	// f[0] is field 3 (state); field n is f[n-3].
	if len(f) < 20 {
		return Stat{}, errors.New("short stat")
	}
	st.State = f[0][0]
	st.PPID, _ = strconv.Atoi(f[1])
	st.StartTime, _ = strconv.ParseUint(f[19], 10, 64)
	return st, nil
}

// Alive reports whether pid is running and, when start is non-zero, is the
// same process that was recorded (guards against pid reuse).
func Alive(pid int, start uint64) bool {
	if pid <= 0 {
		return false
	}
	st, err := ReadStat(pid)
	if err != nil {
		return false
	}
	if st.State == 'Z' || st.State == 'X' {
		return false
	}
	return start == 0 || st.StartTime == start
}

// IsAgentComm reports whether a process name is a supported agent.
func IsAgentComm(comm string) (agent string, ok bool) {
	switch comm {
	case "claude":
		return "claude", true
	case "agy":
		return "agy", true
	}
	return "", false
}

// FindAncestor walks up from pid (inclusive) at most maxDepth levels and
// returns the first process whose comm is want.
func FindAncestor(pid int, want string, maxDepth int) (Stat, bool) {
	for i := 0; i < maxDepth && pid > 1; i++ {
		st, err := ReadStat(pid)
		if err != nil {
			return Stat{}, false
		}
		if st.Comm == want {
			return st, true
		}
		pid = st.PPID
	}
	return Stat{}, false
}

// Environ returns one variable from /proc/<pid>/environ.
func Environ(pid int, key string) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return ""
	}
	prefix := []byte(key + "=")
	for _, kv := range bytes.Split(b, []byte{0}) {
		if bytes.HasPrefix(kv, prefix) {
			return string(kv[len(prefix):])
		}
	}
	return ""
}

// Cmdline returns the argv of pid.
func Cmdline(pid int) []string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
}

// Cwd returns the working directory of pid.
func Cwd(pid int) string {
	p, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(p, " (deleted)")
}

// Table is a snapshot of all processes, for tree searches.
type Table struct {
	ByPID    map[int]Stat
	Children map[int][]int
}

// Snapshot reads every process once.
func Snapshot() Table {
	t := Table{ByPID: map[int]Stat{}, Children: map[int][]int{}}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return t
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		st, err := parseStat(pid, b)
		if err != nil {
			continue
		}
		t.ByPID[pid] = st
		t.Children[st.PPID] = append(t.Children[st.PPID], pid)
	}
	return t
}

// Found is an agent process in a process tree.
type Found struct {
	Stat  Stat
	Agent string
	Depth int // 0 = root itself
}

// AgentsBelow lists every agent process in the tree under root (inclusive),
// breadth-first, with its depth below root.
func (t Table) AgentsBelow(root int) []Found {
	type item struct{ pid, depth int }
	var out []Found
	queue := []item{{root, 0}}
	seen := map[int]bool{}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if seen[it.pid] {
			continue
		}
		seen[it.pid] = true
		if st, ok := t.ByPID[it.pid]; ok {
			if a, ok := IsAgentComm(st.Comm); ok {
				out = append(out, Found{Stat: st, Agent: a, Depth: it.depth})
			}
		}
		for _, c := range t.Children[it.pid] {
			queue = append(queue, item{c, it.depth + 1})
		}
	}
	return out
}

// userHZ is the unit of /proc/<pid>/stat start times. The kernel exports
// these in USER_HZ, which is 100 on Linux regardless of CONFIG_HZ.
const userHZ = 100

// StartWall converts a /proc start time to wall-clock time using btime.
func StartWall(startTicks uint64) time.Time {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "btime ") {
			bt, err := strconv.ParseInt(strings.TrimSpace(line[6:]), 10, 64)
			if err != nil {
				return time.Time{}
			}
			return time.Unix(bt, 0).Add(time.Duration(startTicks) * time.Second / userHZ)
		}
	}
	return time.Time{}
}

// System reads the real /proc.
type System struct{}

// Snapshot reads every process once.
func (System) Snapshot() Table { return Snapshot() }

// Alive reports whether pid runs and is the recorded process.
func (System) Alive(pid int, start uint64) bool { return Alive(pid, start) }

// Cmdline returns the argv of pid.
func (System) Cmdline(pid int) []string { return Cmdline(pid) }

// Cwd returns the working directory of pid.
func (System) Cwd(pid int) string { return Cwd(pid) }

// StartWall converts a start time to wall-clock time.
func (System) StartWall(ticks uint64) time.Time { return StartWall(ticks) }
