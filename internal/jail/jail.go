// Package jail decides the cgroup limits and placement of tmux and its
// workstreams (docs/jails.md). Pure: it reads cgroup paths and settings and
// returns the units to configure and the processes to move; the shell talks
// to systemd.
package jail

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Unit names. A (all of tmux) is Root; workstream slices nest under
// WorkstreamsSlice by systemd's dash naming.
const (
	Root             = "cccp.slice"
	TmuxScope        = "cccp-tmux.scope"
	WorkstreamsSlice = "cccp-ws.slice"
)

// Escape makes a workstream name safe in a unit name. Like systemd-escape
// for unit names, every byte outside [A-Za-z0-9:_.] (and a leading '.')
// becomes \xHH; unlike it, '/' is escaped too (systemd-escape turns it into
// '-'), because a '-' in a slice name is a level of the hierarchy.
func Escape(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ':' || c == '_' || c == '.' && i > 0
		if ok {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

// SliceFor is a workstream's slice.
func SliceFor(workstream string) string { return "cccp-ws-" + Escape(workstream) + ".slice" }

// Limits are a slice's cgroup settings, in systemd's syntax ("" or 0:
// left as they are).
//
// The memory limit is MemoryMax, a hard limit: at it the kernel kills a
// process inside the slice. MemoryHigh, a throttle point, is not used
// ("infinity"): a runaway that crosses it with nowhere to swap is slowed so
// much it never reaches MemoryMax, and hangs, holding its memory, instead
// of failing (measured 2026-11-05; systemd-oomd does not kill it either, as
// the kernel does no reclaim scanning for it). See docs/jails.md.
type Limits struct {
	MemoryHigh    string `toml:"memory_high" json:"memory_high,omitempty"`
	MemoryMax     string `toml:"memory_max" json:"memory_max,omitempty"` // the limit
	MemorySwapMax string `toml:"memory_swap_max" json:"memory_swap_max,omitempty"`
	CPUWeight     int    `toml:"cpu_weight" json:"cpu_weight,omitempty"`
	TasksMax      int    `toml:"tasks_max" json:"tasks_max,omitempty"`
}

// Merge returns l with the fields set in o replacing its own.
func (l Limits) Merge(o Limits) Limits {
	if o.MemoryHigh != "" {
		l.MemoryHigh = o.MemoryHigh
	}
	if o.MemoryMax != "" {
		l.MemoryMax = o.MemoryMax
	}
	if o.MemorySwapMax != "" {
		l.MemorySwapMax = o.MemorySwapMax
	}
	if o.CPUWeight != 0 {
		l.CPUWeight = o.CPUWeight
	}
	if o.TasksMax != 0 {
		l.TasksMax = o.TasksMax
	}
	return l
}

// Presets.
const (
	PresetNormal = "normal"
	PresetBoost  = "boost"
	PresetMax    = "max"
)

// Presets lists the presets in the order offered.
var Presets = []string{PresetNormal, PresetBoost, PresetMax}

// Defaults are the built-in limits (docs/jails.md): A, A while a workstream
// is at max, and each workstream preset.
var Defaults = Settings{
	Root:    Limits{MemoryHigh: "infinity", MemoryMax: "80%", MemorySwapMax: "2G", CPUWeight: 50},
	RootMax: Limits{MemoryHigh: "infinity", MemoryMax: "90%", MemorySwapMax: "4G", CPUWeight: 200},
	Presets: map[string]Limits{
		PresetNormal: {MemoryHigh: "infinity", MemoryMax: "47%", MemorySwapMax: "1G", CPUWeight: 100, TasksMax: 4096},
		PresetBoost:  {MemoryHigh: "infinity", MemoryMax: "80%", MemorySwapMax: "2G", CPUWeight: 400, TasksMax: 8192},
		PresetMax:    {MemoryHigh: "infinity", MemoryMax: "90%", MemorySwapMax: "4G", CPUWeight: 400, TasksMax: 8192},
	},
}

// Warn and Rearm are the shares of a workstream's memory limit at which
// cccp warns that it is near (once), and below which it may warn again.
const (
	WarnAt = 0.85
	Rearm  = 0.75
)

// Settings are the limits in force: the built-in ones with the config's
// changes. Workstreams maps a workstream to its own normal limits (a
// preset's name, or custom values over normal).
type Settings struct {
	Root        Limits
	RootMax     Limits
	Presets     map[string]Limits
	Workstreams map[string]WorkstreamSetting
}

// WorkstreamSetting is one workstream's configured limits: a preset, and/or
// custom values on top of it.
type WorkstreamSetting struct {
	Preset string
	Custom Limits
}

// Boost is a workstream's temporary preset. A zero Until lasts until it is
// undone.
type Boost struct {
	Preset string    `json:"preset"`
	Until  time.Time `json:"until,omitzero"`
}

// Active reports whether the boost still applies at now.
func (b Boost) Active(now time.Time) bool { return b.Until.IsZero() || now.Before(b.Until) }

// Expire returns the boosts still active at now.
func Expire(boosts map[string]Boost, now time.Time) map[string]Boost {
	out := map[string]Boost{}
	for ws, b := range boosts {
		if b.Active(now) {
			out[ws] = b
		}
	}
	return out
}

// Deadline is when the next boost expires (zero: none will).
func Deadline(boosts map[string]Boost, now time.Time) time.Time {
	var t time.Time
	for _, b := range boosts {
		if !b.Until.IsZero() && b.Until.After(now) && (t.IsZero() || b.Until.Before(t)) {
			t = b.Until
		}
	}
	return t
}

// Effective is a workstream's limits and the preset they come from:
// an active boost, else its configured setting, else normal.
func (s Settings) Effective(ws string, boosts map[string]Boost, now time.Time) (Limits, string) {
	if b, ok := boosts[ws]; ok && b.Active(now) {
		if l, ok := s.Presets[b.Preset]; ok {
			return l, b.Preset
		}
	}
	set := s.Workstreams[ws]
	preset := set.Preset
	l, ok := s.Presets[preset]
	if !ok {
		preset, l = PresetNormal, s.Presets[PresetNormal]
	}
	if set.Custom != (Limits{}) {
		return l.Merge(set.Custom), "custom"
	}
	return l, preset
}

// Desired is every unit's limits: A (raised while any workstream is at
// max), and each workstream's slice.
func (s Settings) Desired(workstreams []string, boosts map[string]Boost, now time.Time) map[string]Limits {
	out := map[string]Limits{Root: s.Root}
	for _, ws := range workstreams {
		l, preset := s.Effective(ws, boosts, now)
		out[SliceFor(ws)] = l
		if preset == PresetMax {
			out[Root] = s.RootMax
		}
	}
	return out
}

// Pane is a tmux pane and the cgroup its process is in.
type Pane struct {
	ID         string // %N
	PID        int    // its process (pane_pid)
	Workstream string // its tmux session
	Cgroup     string // of its pid (/proc/PID/cgroup), "" if unknown
}

// Move is one move to make, into a new scope in Slice: for a pane, its
// processes (all of cgroup From when Whole: the cgroup is the pane's alone;
// else the pane's process and its descendants); for the tmux server (Pane
// ""), the server process alone (its panes are placed one by one).
type Move struct {
	Pane  string
	PID   int
	From  string
	Slice string
	Whole bool
}

// Placement returns the moves that put the tmux server in TmuxScope and
// each pane in its workstream's slice. Processes are left alone when their
// cgroup is unknown.
func Placement(serverPID int, serverCgroup string, panes []Pane) []Move {
	var out []Move
	if serverCgroup != "" && !strings.HasSuffix(serverCgroup, "/"+Root+"/"+TmuxScope) {
		out = append(out, Move{PID: serverPID, From: serverCgroup, Slice: Root})
	}
	// A cgroup can be moved whole only if it holds one pane and not the
	// server (tmux built without systemd support keeps every pane in the
	// server's cgroup).
	uses := map[string]int{serverCgroup: 1}
	for _, p := range panes {
		uses[p.Cgroup]++
	}
	for _, p := range panes {
		if p.Cgroup == "" || p.Workstream == "" {
			continue
		}
		slice := SliceFor(p.Workstream)
		if !strings.Contains(p.Cgroup, "/"+slice+"/") {
			out = append(out, Move{Pane: p.ID, PID: p.PID, From: p.Cgroup, Slice: slice, Whole: uses[p.Cgroup] == 1})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Pane < out[j].Pane })
	return out
}

// Stats are a slice's live numbers.
type Stats struct {
	Memory     int64 // bytes in use (memory.current)
	High, Max  int64 // throttle and kill points in bytes (0: none)
	Throttled  int64 // times over the throttle point (memory.events high)
	OOMKills   int64 // processes killed at the kill point (memory.events oom_kill)
	CPUUsecs   int64 // CPU time used (cpu.stat usage_usec)
	Unreadable bool
}

// ParseKeyed parses "key value" lines (memory.events, cpu.stat).
func ParseKeyed(s string) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			var n int64
			if _, err := fmt.Sscan(f[1], &n); err == nil {
				out[f[0]] = n
			}
		}
	}
	return out
}

// ParseBytes parses memory.current, memory.high or memory.max ("max": no
// limit, 0).
func ParseBytes(s string) int64 {
	var n int64
	if _, err := fmt.Sscan(strings.TrimSpace(s), &n); err != nil {
		return 0
	}
	return n
}
