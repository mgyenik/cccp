package cgroups

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cccp/internal/jail"
)

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fake(t *testing.T) FS {
	d := t.TempDir()
	return FS{Sys: filepath.Join(d, "sys"), Proc: filepath.Join(d, "proc"), UserRoot: "/user.slice/user-1000.slice/user@1000.service"}
}

func TestAvailable(t *testing.T) {
	f := fake(t)
	if err := f.Available(); err == nil || !strings.Contains(err.Error(), "v2") {
		t.Fatalf("no cgroup2: %v", err)
	}
	write(t, filepath.Join(f.Sys, "cgroup.controllers"), "cpuset cpu io memory pids\n")
	write(t, filepath.Join(f.Sys, f.UserRoot, "cgroup.subtree_control"), "cpu pids\n")
	if err := f.Available(); err == nil || !strings.Contains(err.Error(), "memory") {
		t.Fatalf("memory not delegated: %v", err)
	}
	write(t, filepath.Join(f.Sys, f.UserRoot, "cgroup.subtree_control"), "cpu memory pids\n")
	if err := f.Available(); err != nil {
		t.Fatal(err)
	}
}

func TestCgroupProcsAndStats(t *testing.T) {
	f := fake(t)
	write(t, filepath.Join(f.Proc, "42", "cgroup"), "0::/user.slice/x/tmux-spawn-a.scope\n")
	if got := f.Cgroup(42); got != "/user.slice/x/tmux-spawn-a.scope" {
		t.Fatal(got)
	}
	if f.Cgroup(43) != "" {
		t.Fatal("unknown process")
	}
	write(t, filepath.Join(f.Sys, "/user.slice/x/tmux-spawn-a.scope", "cgroup.procs"), "42\n43\n")
	if got := f.Procs("/user.slice/x/tmux-spawn-a.scope"); !reflect.DeepEqual(got, []int{42, 43}) {
		t.Fatal(got)
	}
	slice := jail.SliceFor("my-ws")
	dir := filepath.Join(f.Sys, f.UserRoot, "cccp.slice", "cccp-ws.slice", slice)
	write(t, filepath.Join(dir, "memory.current"), "1048576\n")
	write(t, filepath.Join(dir, "memory.high"), "2097152\n")
	write(t, filepath.Join(dir, "memory.max"), "max\n")
	write(t, filepath.Join(dir, "memory.events"), "low 0\nhigh 7\nmax 1\noom 1\noom_kill 2\n")
	write(t, filepath.Join(dir, "cpu.stat"), "usage_usec 5000\nuser_usec 3000\n")
	want := jail.Stats{Memory: 1 << 20, High: 2 << 20, Max: 0, Throttled: 7, OOMKills: 2, CPUUsecs: 5000}
	if got := f.Stats(slice); got != want {
		t.Fatalf("%+v", got)
	}
	if f.Stats(jail.SliceFor("gone")) != (jail.Stats{}) {
		t.Fatal("a slice that does not exist has no numbers")
	}
	// One that exists but cannot be read is unreadable (not zero: zeros
	// would make the next good read replay its kills).
	if err := os.MkdirAll(filepath.Join(f.Sys, f.SlicePath(jail.SliceFor("odd"))), 0o755); err != nil {
		t.Fatal(err)
	}
	if !f.Stats(jail.SliceFor("odd")).Unreadable {
		t.Fatal("exists, memory.current missing")
	}
	if f.SlicePath(jail.Root) != f.UserRoot+"/cccp.slice" {
		t.Fatal(f.SlicePath(jail.Root))
	}
}

func TestMemTotal(t *testing.T) {
	f := fake(t)
	write(t, filepath.Join(f.Proc, "meminfo"), "MemTotal:       90193452 kB\nMemFree:  1 kB\n")
	if got := f.MemTotal(); got != 90193452<<10 {
		t.Fatal(got)
	}
}
