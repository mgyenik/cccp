package present

import (
	"testing"
	"time"

	"cccp/internal/core"
	"cccp/internal/model"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func TestAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "0s", 45 * time.Second: "45s", 12 * time.Minute: "12m",
		3 * time.Hour: "3h", 47 * time.Hour: "47h", 72 * time.Hour: "3d",
	} {
		if got := Age(d); got != want {
			t.Errorf("%v → %q want %q", d, got, want)
		}
	}
}

func TestCacheText(t *testing.T) {
	idle := core.Session{Group: core.GroupIdle, Cache: core.Cache{Known: true, ExpiresAt: t0.Add(42 * time.Minute)}}
	if got := CacheText(idle, t0); got != "warm 42m" {
		t.Fatal(got)
	}
	idle.Cache.TTLAssumed = true
	idle.Cache.ExpiresAt = t0.Add(-3 * time.Hour)
	if got := CacheText(idle, t0); got != "cold 3h?" {
		t.Fatal(got)
	}
	if got := CacheText(core.Session{Group: core.GroupWorking, Cache: idle.Cache}, t0); got != "" {
		t.Fatalf("working sessions show no cache state: %q", got)
	}
}

func TestStatusSegmentAndGlyphs(t *testing.T) {
	snap := core.Snapshot{Sessions: []core.Session{
		{Key: "a", Group: core.GroupNeedsYou, Activity: model.NeedsApproval, WindowID: "@1"},
		{Key: "b", Group: core.GroupWorking, WindowID: "@1"},
		{Key: "c", Group: core.GroupCold, WindowID: "@2"},
		{Key: "d", Group: core.GroupClosed, WindowID: "@3"},
	}}
	if got := StatusSegment(snap); got != "#[fg=yellow,bold]⚠1#[default] #[fg=cyan]●1#[default] #[fg=blue]❄1#[default]" {
		t.Fatal(got)
	}
	if got := StatusSegment(core.Snapshot{}); got != "" {
		t.Fatalf("empty: %q", got)
	}
	g := WindowGlyphs(snap)
	if g["@1"] != "#[fg=yellow,bold]⚠#[default] " || g["@2"] != "#[fg=blue]❄#[default] " {
		t.Fatalf("the most urgent session decides a window's glyph: %q", g)
	}
	if _, ok := g["@3"]; ok {
		t.Fatal("ended sessions do not decorate windows")
	}
}

func TestDetailPriority(t *testing.T) {
	cases := []struct {
		s    core.Session
		want string
	}{
		{core.Session{Activity: model.Errored, Error: "rate_limit"}, "error: rate_limit"},
		{core.Session{Question: "Allow Bash: make"}, "Allow Bash: make"},
		{core.Session{Activity: model.Working, Tool: "Bash", ToolDetail: "go test"}, "Bash: go test"},
		{core.Session{Group: core.GroupYourTurn, Summary: "Approve the release"}, "Approve the release"},
		{core.Session{Group: core.GroupYourTurn, LastMessage: "Done.\n\nShip it?\n```"}, "Ship it?"},
		{core.Session{LastPrompt: "fix\nthe bug"}, "› fix the bug"},
		{core.Session{Group: core.GroupWorking, BackgroundShell: true, Summary: "Build running"}, "background shell · Build running"},
		{core.Session{Group: core.GroupWorking, BackgroundShell: true, LastMessage: "Started it.\nI'll report back."}, "background shell · I'll report back."},
		{core.Session{Group: core.GroupYourTurn, BackgroundShell: true, Summary: "Try the dev server"}, "Try the dev server"},
		{core.Session{Notes: []string{"no transcript found"}}, "no transcript found"},
	}
	for i, c := range cases {
		if got := Detail(c.s); got != c.want {
			t.Errorf("case %d: %q want %q", i, got, c.want)
		}
	}
}

func TestProfiles(t *testing.T) {
	one := core.Snapshot{Sessions: []core.Session{{Profile: "work"}, {Profile: "work"}, {}}}
	two := core.Snapshot{Sessions: []core.Session{{Profile: "work"}, {Profile: "personal"}}}
	if MultiProfile(one) || !MultiProfile(two) {
		t.Fatal("MultiProfile")
	}
	s := core.Session{Profile: "workstation", Title: "Deploy"}
	if got := ProfileTag(s, 8); got != "worksta…" {
		t.Fatal(got)
	}
	if got := TitleWithProfile(s, true); got != "[workstation] Deploy" {
		t.Fatal(got)
	}
	if got := TitleWithProfile(s, false); got != "Deploy" {
		t.Fatal(got)
	}
	if got := TitleWithProfile(core.Session{Title: "agy"}, true); got != "agy" {
		t.Fatal("no tag without a profile")
	}
}

func TestTurns(t *testing.T) {
	for n, want := range map[int]string{0: "", -1: "", 1: "1 turn", 2: "2 turns", 30: "30 turns"} {
		if got := Turns(n); got != want {
			t.Errorf("%d → %q want %q", n, got, want)
		}
	}
}

func TestAroundKeepsTheMatchVisible(t *testing.T) {
	s := "…which completes the story: it is the Viterbi path that we keep"
	if got := Around(s, []string{"viterbi"}, 12); got != "…it is the Viterbi path that we keep" {
		t.Fatalf("%q", got)
	}
	// The earliest of several terms, starting at the word start in the lead.
	if got := Around("aaaaaaaaaaaaaaaaaaaa path then Viterbi", []string{"viterbi", "path"}, 4); got != "…path then Viterbi" {
		t.Fatalf("%q", got)
	}
	// No word start in the lead: cut lead runes before the match.
	if got := Around("aaaaaaaaaaaaaaaaaaaapath then", []string{"path"}, 4); got != "…aaaapath then" {
		t.Fatalf("%q", got)
	}
	// A match near the start, or none, leaves the snippet as it was.
	if got := Around("Viterbi first", []string{"viterbi"}, 12); got != "Viterbi first" {
		t.Fatalf("%q", got)
	}
	if got := Around("…no match\nhere", []string{"zzz"}, 12); got != "…no match here" {
		t.Fatalf("%q", got)
	}
	// Case folding keeps rune positions: the cut lands on the right rune.
	if got := Around("ÀÀÀÀÀÀÀÀÀÀ x ÉCOLE", []string{"école"}, 3); got != "…x ÉCOLE" {
		t.Fatalf("%q", got)
	}
}

func TestCacheTextForAnEstimatedCache(t *testing.T) {
	last := t0.Add(-time.Minute)
	s := core.Session{Group: core.GroupIdle, Cache: core.Cache{Known: true, Fades: true, LastActivity: last,
		CoolingAt: last.Add(core.AgyWarmFor), ExpiresAt: last.Add(core.AgyColdAfter)}}
	for after, want := range map[time.Duration]string{
		time.Minute:                   "warm?",
		core.AgyWarmFor:               "cooling?",
		core.AgyColdAfter + time.Hour: "cold 1h?",
	} {
		if got := CacheText(s, last.Add(after)); got != want {
			t.Errorf("after %v: %q want %q", after, got, want)
		}
	}
}

func TestBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "-", 4096: "4 KiB", 812 << 20: "812 MiB", 13207024435: "12.3 GiB"} {
		if got := Bytes(n); got != want {
			t.Errorf("%d: %q want %q", n, got, want)
		}
	}
}
