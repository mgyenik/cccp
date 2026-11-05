package ui

import (
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"cccp/internal/core"
	"cccp/internal/jail"
)

func TestSampleCPUAndNearLimit(t *testing.T) {
	t0 := time.Unix(1000, 0)
	s := sample(jailSample{}, jail.Stats{CPUUsecs: 1_000_000, Memory: 80, Max: 100}, t0)
	if s.cpuKnown || s.nearLimit() {
		t.Fatal("one read: no CPU yet; 80% is not near")
	}
	s = sample(s, jail.Stats{CPUUsecs: 5_000_000, Memory: 85, Max: 100}, t0.Add(2*time.Second))
	if !s.cpuKnown || s.cpu != 2 || !s.nearLimit() {
		t.Fatalf("4 s of CPU over 2 s: 2 cores; 85%% is near: %+v", s)
	}
	if (jailSample{stats: jail.Stats{Memory: 1 << 40}}).nearLimit() {
		t.Fatal("no limit: never near it")
	}
}

func TestWarningBelowCurrentUse(t *testing.T) {
	total := int64(100 << 30)
	if w := warning(jail.Limits{MemoryMax: "infinity"}, total, 99<<30); w != "" {
		t.Fatalf("no limit, no warning: %q", w)
	}
	if w := warning(jail.Limits{MemoryMax: "47%"}, total, 50<<30); !strings.Contains(w, "kill a process") {
		t.Fatal("47 GiB below 50 GiB in use")
	}
	if w := warning(jail.Limits{MemoryMax: "80%"}, total, 50<<30); w != "" {
		t.Fatal(w)
	}
}

// A workstream with its own configured limits: its normal row shows those
// (what ending a boost returns it to), not the normal preset's.
func TestLimiterNormalIsTheWorkstreamsOwn(t *testing.T) {
	j := jailSnap().Jail
	ws := j.Workstreams[0]
	ws.Normal, ws.NormalPreset = jail.Limits{MemoryMax: "64M", CPUWeight: 100}, "custom"
	l := newLimiter(ws, j, 100<<20)
	out := ansi.Strip(l.view(120, 20))
	if !strings.Contains(out, "normal (custom)      memory 64 MiB ") || !strings.Contains(out, "kill a process") {
		t.Fatalf("%s", out)
	}
	if !strings.Contains(out, "boost for 1h         memory 80.0 GiB") {
		t.Fatalf("a boost is the preset's limits:\n%s", out)
	}
	ws.Normal, ws.NormalPreset = j.Presets[jail.PresetNormal], jail.PresetNormal
	if out := ansi.Strip(newLimiter(ws, j, 0).view(120, 20)); !strings.Contains(out, "  normal               memory 47.0 GiB") {
		t.Fatalf("%s", out)
	}
}

func jailSnap() core.Snapshot {
	snap := snapshot()
	snap.Sessions[0].TmuxSess = "infra"
	snap.Jail = core.Jail{On: true, MemTotal: 100 << 30,
		Presets: map[string]jail.Limits{jail.PresetNormal: {MemoryMax: "47%", CPUWeight: 100},
			jail.PresetBoost: {MemoryMax: "80%", CPUWeight: 400}, jail.PresetMax: {MemoryMax: "90%", CPUWeight: 400}},
		Workstreams: []core.JailWorkstream{{Name: "infra", Slice: "cccp-ws-infra.slice", Preset: jail.PresetBoost, Boosted: true,
			Until: time.Now().Add(47 * time.Minute), Stats: jail.Stats{Memory: 12 << 30, Max: 80 << 30}}}}
	return snap
}

func TestCockpitLimits(t *testing.T) {
	m, _, src := newModel(t)
	type call struct {
		ws, preset string
		d          time.Duration
	}
	var calls []call
	m.opt.Actions.SetLimits = func(ws, preset string, d time.Duration) error {
		calls = append(calls, call{ws, preset, d})
		return nil
	}
	m.Update(snapMsg(jailSnap()))
	m.preview = previewConversation
	out := ansi.Strip(m.render())
	if !strings.Contains(out, "⇧infra") {
		t.Fatalf("boosted workstream marked in its column:\n%s", out)
	}
	if !strings.Contains(out, "jail infra · ⇧ boost, 4") || !strings.Contains(out, "memory 12.0 GiB of 80.0 GiB") {
		t.Fatalf("preview's jail line:\n%s", out)
	}
	key(m, "L")
	if m.mode != modeLimits || !strings.Contains(ansi.Strip(m.render()), "boost for 2h") {
		t.Fatal("L opens the picker")
	}
	key(m, "j")
	key(m, "j") // boost for 2h
	key(m, "enter")
	if !reflect.DeepEqual(calls, []call{{"infra", jail.PresetBoost, 2 * time.Hour}}) || m.quitting || m.mode != modeList {
		t.Fatalf("calls %v quitting %v mode %v", calls, m.quitting, m.mode)
	}
	if !strings.Contains(m.status, "infra: boost for 2h") || src.refreshed != 1 {
		t.Fatalf("status %q refreshed %d", m.status, src.refreshed)
	}
	// Esc cancels.
	key(m, "L")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.mode != modeList || len(calls) != 1 {
		t.Fatal("esc")
	}
}

func TestLimitsWhenJailsAreOffOrNoWorkstream(t *testing.T) {
	m, _, _ := newModel(t)
	m.opt.Actions.SetLimits = func(string, string, time.Duration) error { return nil }
	snap := snapshot()
	snap.Jail = core.Jail{Why: "no memory controller"}
	m.Update(snapMsg(snap))
	key(m, "L")
	if m.mode != modeList || !strings.Contains(m.status, "jails are off: no memory controller") {
		t.Fatalf("mode %v status %q", m.mode, m.status)
	}
	m.Update(snapMsg(jailSnap()))
	key(m, "j") // a session in no workstream
	key(m, "L")
	if m.mode != modeList || !strings.Contains(m.status, "not in a running workstream") {
		t.Fatalf("status %q", m.status)
	}
}

func TestJailNumbersAreReadWhileOpen(t *testing.T) {
	m, _, _ := newModel(t)
	reads := 0
	m.opt.JailStats = func(slice string) jail.Stats {
		reads++
		return jail.Stats{Memory: 1 << 30, CPUUsecs: int64(reads) * 1_000_000}
	}
	m.Update(snapMsg(jailSnap()))
	now := time.Now()
	for i := 0; i < 3; i++ {
		_, cmd := m.Update(tickMsg(now.Add(time.Duration(i) * jailRead)))
		if cmd != nil {
			if msg, ok := findMsg[jailStatsMsg](cmd); ok {
				m.Update(msg)
			}
		}
	}
	if reads == 0 || !m.jail["cccp-ws-infra.slice"].cpuKnown {
		t.Fatalf("reads %d sample %+v", reads, m.jail["cccp-ws-infra.slice"])
	}
}

// findMsg runs a command's batch, returning the first message of type T
// (without running timers: only commands returning T are expected here).
func findMsg[T any](cmd tea.Cmd) (T, bool) {
	var zero T
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		for _, c := range b {
			if c == nil {
				continue
			}
			if v, ok := runQuick(c).(T); ok {
				return v, true
			}
		}
		return zero, false
	}
	v, ok := msg.(T)
	return v, ok
}

// runQuick runs a command unless it is a timer (tick, cursor blink), which
// would sleep.
func runQuick(c tea.Cmd) tea.Msg {
	done := make(chan tea.Msg, 1)
	go func() { done <- c() }()
	select {
	case m := <-done:
		return m
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}
