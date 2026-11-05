// Package cgroups reads cgroups v2: which cgroup a process is in, the
// processes in a cgroup, and a cccp slice's numbers (docs/jails.md).
package cgroups

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"cccp/internal/jail"
)

// FS reads the cgroup filesystem. UserRoot is the user manager's cgroup
// (/user.slice/user-UID.slice/user@UID.service); Sys and Proc are the
// mount points (/sys/fs/cgroup, /proc), set otherwise only in tests.
type FS struct {
	Sys, Proc string
	UserRoot  string
}

// New is the real filesystem for user uid.
func New(uid int) FS {
	return FS{Sys: "/sys/fs/cgroup", Proc: "/proc",
		UserRoot: fmt.Sprintf("/user.slice/user-%d.slice/user@%d.service", uid, uid)}
}

// Available reports why jails cannot work here, or nil: cgroups v2, and
// the cpu, memory and pids controllers delegated to the user's manager.
func (f FS) Available() error {
	if _, err := os.Stat(filepath.Join(f.Sys, "cgroup.controllers")); err != nil {
		return errors.New("cgroups v2 is not mounted at " + f.Sys)
	}
	b, err := os.ReadFile(filepath.Join(f.Sys, f.UserRoot, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("no systemd user manager cgroup: %v", err)
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(string(b)) {
		have[c] = true
	}
	var missing []string
	for _, c := range []string{"cpu", "memory", "pids"} {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the user manager has no %s controller (delegated: %s)", strings.Join(missing, ", "), strings.TrimSpace(string(b)))
	}
	return nil
}

// Cgroup is the cgroup pid is in ("" if unknown).
func (f FS) Cgroup(pid int) string {
	b, err := os.ReadFile(filepath.Join(f.Proc, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return p
		}
	}
	return ""
}

// Procs are the processes in a cgroup (a path as Cgroup returns).
func (f FS) Procs(cgroup string) []int {
	b, err := os.ReadFile(filepath.Join(f.Sys, cgroup, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var out []int
	for _, s := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// SlicePath is a cccp slice's cgroup: A, or a workstream's slice under it.
func (f FS) SlicePath(slice string) string {
	if slice == jail.Root {
		return filepath.Join(f.UserRoot, jail.Root)
	}
	return filepath.Join(f.UserRoot, jail.Root, jail.WorkstreamsSlice, slice)
}

// Stats reads a cccp slice's numbers. A slice that does not exist (yet)
// has none: zero, not unreadable, so every kill once it exists counts as
// new (a kill seconds after the slice is made is still reported).
func (f FS) Stats(slice string) jail.Stats {
	dir := filepath.Join(f.Sys, f.SlicePath(slice))
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return jail.Stats{}
	}
	read := func(name string) (string, bool) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		return string(b), err == nil
	}
	cur, ok := read("memory.current")
	if !ok {
		return jail.Stats{Unreadable: true}
	}
	s := jail.Stats{Memory: jail.ParseBytes(cur)}
	if v, ok := read("memory.high"); ok {
		s.High = jail.ParseBytes(v)
	}
	if v, ok := read("memory.max"); ok {
		s.Max = jail.ParseBytes(v)
	}
	if v, ok := read("memory.events"); ok {
		ev := jail.ParseKeyed(v)
		s.Throttled, s.OOMKills = ev["high"], ev["oom_kill"]
	}
	if v, ok := read("cpu.stat"); ok {
		s.CPUUsecs = jail.ParseKeyed(v)["usage_usec"]
	}
	return s
}

// EventsFile is a slice's memory.events, watched for kills at the limit.
func (f FS) EventsFile(slice string) string {
	return filepath.Join(f.Sys, f.SlicePath(slice), "memory.events")
}

// MemTotal is the machine's memory in bytes (/proc/meminfo), 0 if unknown.
func (f FS) MemTotal() int64 {
	b, err := os.ReadFile(filepath.Join(f.Proc, "meminfo"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(rest)
			if len(f) == 2 && f[1] == "kB" {
				if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
					return n << 10
				}
			}
		}
	}
	return 0
}
