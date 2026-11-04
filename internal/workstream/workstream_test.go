package workstream

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"cccp/internal/model"
)

var (
	t0    = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	srvA  = Server{PID: 100, Start: 1000, Socket: "/tmp/tmux-1000/default"}
	srvB  = Server{PID: 200, Start: 2000, Socket: "/tmp/tmux-1000/default"} // the same socket, restarted
	convA = &Conversation{Agent: model.Claude, SessionID: "c1", ConfigDir: "/h/.claude-work", Title: "Refactor"}
)

func pane(sess, sid string, win, idx int, path string) LivePane {
	return LivePane{Pane: model.TmuxPane{ID: "%" + sid + path, Session: sess, SessionID: sid, WindowIndex: win,
		WindowName: "w" + string(rune('0'+win)), PaneIndex: idx, Path: path, WindowLayout: "b25d,80x24,0,0,0",
		WindowAutoName: true}}
}

func live(srv Server, panes ...LivePane) Live {
	return Live{OK: true, Running: true, Server: srv, Panes: panes}
}

func names(r Record) []string {
	var out []string
	for _, w := range r.Workstreams {
		n := w.Name
		if w.Dormant {
			n += "(dormant)"
		}
		out = append(out, n)
	}
	return out
}

func TestPanesAreGroupedInOrder(t *testing.T) {
	p1 := pane("infra", "$1", 2, 1, "/w/b")
	p1.Conv = convA
	p2 := pane("infra", "$1", 2, 0, "/w/a")
	p2.Pane.PaneActive, p2.Pane.WindowActive = true, true
	p3 := pane("infra", "$1", 1, 0, "/w/c")
	p3.Pane.WindowAutoName, p3.Pane.WindowName = false, "logs"
	p4 := pane("docs", "$2", 0, 0, "/w/d")
	p4.Pane.SessionActivity = t0
	rec := Observe(Record{}, live(srvA, p1, p2, p3, p4), t0)
	if got := names(rec); !reflect.DeepEqual(got, []string{"docs", "infra"}) {
		t.Fatalf("sessions %v", got)
	}
	infra, _ := rec.Find("infra")
	if infra.ID != "$1" || infra.Server != srvA || len(infra.Windows) != 2 {
		t.Fatalf("%+v", infra)
	}
	w1, w2 := infra.Windows[0], infra.Windows[1]
	if w1.Index != 1 || w1.Name != "logs" || w1.AutoName || w2.Index != 2 || !w2.Active {
		t.Fatalf("windows %+v %+v", w1, w2)
	}
	// Panes in pane-index order, whatever order tmux listed them in.
	if len(w2.Panes) != 2 || w2.Panes[0].Cwd != "/w/a" || !w2.Panes[0].Active || w2.Panes[1].Cwd != "/w/b" {
		t.Fatalf("panes %+v", w2.Panes)
	}
	if p := w2.Panes[1]; p.Kind != KindConversation || p.Conv.SessionID != "c1" {
		t.Fatalf("conversation pane %+v", p)
	}
	if w2.Panes[0].Kind != KindShell {
		t.Fatal("other panes are shells")
	}
	if docs, _ := rec.Find("docs"); !docs.LastUsed.Equal(t0) {
		t.Fatal("last use")
	}
}

func TestClosedOnARunningServerIsForgottenAfterGrace(t *testing.T) {
	rec := Observe(Record{}, live(srvA, pane("infra", "$1", 0, 0, "/w"), pane("docs", "$2", 0, 0, "/w")), t0)
	// infra's last window closed: the server runs on without it.
	rec = Observe(rec, live(srvA, pane("docs", "$2", 0, 0, "/w")), t0.Add(time.Second))
	if len(rec.Pending) != 1 || rec.Pending[0].Name != "infra" || !rec.Pending[0].Since.Equal(t0.Add(time.Second)) {
		t.Fatalf("pending %+v", rec.Pending)
	}
	if got := names(rec); !reflect.DeepEqual(got, []string{"docs", "infra"}) {
		t.Fatalf("kept while pending, not dormant: %v", got)
	}
	if !rec.Deadline().Equal(t0.Add(time.Second + Grace)) {
		t.Fatalf("deadline %v", rec.Deadline())
	}
	// Still gone just before the grace ends: still pending, since unchanged.
	rec = Observe(rec, live(srvA, pane("docs", "$2", 0, 0, "/w")), t0.Add(Grace))
	if len(rec.Pending) != 1 || !rec.Pending[0].Since.Equal(t0.Add(time.Second)) {
		t.Fatalf("pending %+v", rec.Pending)
	}
	// Gone for the whole grace: forgotten.
	rec = Observe(rec, live(srvA, pane("docs", "$2", 0, 0, "/w")), t0.Add(time.Second+Grace))
	if got := names(rec); !reflect.DeepEqual(got, []string{"docs"}) || len(rec.Pending) != 0 {
		t.Fatalf("not forgotten: %v %+v", got, rec.Pending)
	}
	if !rec.Deadline().IsZero() {
		t.Fatal("no deadline left")
	}
}

func TestShutdownForgetsNothing(t *testing.T) {
	rec := Observe(Record{}, live(srvA, pane("infra", "$1", 0, 0, "/w"), pane("docs", "$2", 0, 0, "/w")), t0)
	// Shutting down: sessions vanish one by one while the server still runs.
	rec = Observe(rec, live(srvA, pane("docs", "$2", 0, 0, "/w")), t0.Add(time.Second))
	// Then the server is gone.
	rec = Observe(rec, Live{OK: true, Running: false}, t0.Add(2*time.Second))
	if got := names(rec); !reflect.DeepEqual(got, []string{"docs(dormant)", "infra(dormant)"}) || len(rec.Pending) != 0 {
		t.Fatalf("%v pending %+v", got, rec.Pending)
	}
	// Long after, still nothing forgotten; and a new server does not
	// revive the old pending mark.
	rec = Observe(rec, live(srvB, pane("scratch", "$0", 0, 0, "/w")), t0.Add(time.Hour))
	if got := names(rec); !reflect.DeepEqual(got, []string{"docs(dormant)", "infra(dormant)", "scratch"}) || len(rec.Pending) != 0 {
		t.Fatalf("%v pending %+v", got, rec.Pending)
	}
}

func TestPendingWhenTheServerRestartsIsDropped(t *testing.T) {
	rec := Observe(Record{}, live(srvA, pane("infra", "$1", 0, 0, "/w"), pane("docs", "$2", 0, 0, "/w")), t0)
	rec = Observe(rec, live(srvA, pane("docs", "$2", 0, 0, "/w")), t0.Add(time.Second))
	// The daemon missed the server going away: the next look finds a new
	// server, long after the grace.
	rec = Observe(rec, live(srvB, pane("other", "$0", 0, 0, "/w")), t0.Add(time.Minute))
	if got := names(rec); !reflect.DeepEqual(got, []string{"docs(dormant)", "infra(dormant)", "other"}) || len(rec.Pending) != 0 {
		t.Fatalf("%v pending %+v", got, rec.Pending)
	}
}

func TestComingBackCancelsPending(t *testing.T) {
	rec := Observe(Record{}, live(srvA, pane("infra", "$1", 0, 0, "/w")), t0)
	rec = Observe(rec, live(srvA, pane("docs", "$2", 0, 0, "/w")), t0.Add(time.Second))
	if len(rec.Pending) != 1 {
		t.Fatal("infra should be pending")
	}
	rec = Observe(rec, live(srvA, pane("infra", "$1", 0, 0, "/w"), pane("docs", "$2", 0, 0, "/w")), t0.Add(2*time.Second))
	if len(rec.Pending) != 0 {
		t.Fatalf("pending %+v", rec.Pending)
	}
}

func TestUnreadableServerChangesNothing(t *testing.T) {
	rec := Observe(Record{}, live(srvA, pane("infra", "$1", 0, 0, "/w")), t0)
	rec = Observe(rec, live(srvA, pane("docs", "$2", 0, 0, "/w")), t0.Add(time.Second))
	before := rec.Clone()
	rec = Observe(rec, Live{OK: false}, t0.Add(time.Hour))
	if !rec.Equal(before) {
		t.Fatalf("changed: %+v", rec)
	}
}

func TestRenameKeepsOneRecord(t *testing.T) {
	rec := Observe(Record{}, live(srvA, pane("infra", "$1", 0, 0, "/w")), t0)
	rec = Observe(rec, live(srvA, pane("platform", "$1", 0, 0, "/w")), t0.Add(time.Second))
	if got := names(rec); !reflect.DeepEqual(got, []string{"platform"}) || len(rec.Pending) != 0 {
		t.Fatalf("%v pending %+v", got, rec.Pending)
	}
}

func TestARunningSessionReplacesADormantOneOfTheSameName(t *testing.T) {
	rec := Observe(Record{}, live(srvA, pane("infra", "$1", 0, 0, "/old")), t0)
	rec = Observe(rec, Live{OK: true}, t0.Add(time.Second))
	rec = Observe(rec, live(srvB, pane("infra", "$0", 0, 0, "/new")), t0.Add(time.Minute))
	w, _ := rec.Find("infra")
	if len(rec.Workstreams) != 1 || w.Dormant || w.Windows[0].Panes[0].Cwd != "/new" {
		t.Fatalf("%+v", rec.Workstreams)
	}
}

func TestDormantOrderAndClone(t *testing.T) {
	r := Record{Workstreams: []Workstream{
		{Name: "a", Dormant: true, LastUsed: t0},
		{Name: "b"},
		{Name: "c", Dormant: true, LastUsed: t0.Add(time.Hour), Windows: []Window{{Panes: []Pane{{Conv: Conversation{SessionID: "x"}}}}}},
	}}
	if d := r.Dormant(); len(d) != 2 || d[0].Name != "c" || d[1].Name != "a" {
		t.Fatalf("%+v", d)
	}
	c := r.Clone()
	c.Workstreams[2].Windows[0].Panes[0].Conv.SessionID = "changed"
	if r.Workstreams[2].Windows[0].Panes[0].Conv.SessionID != "x" {
		t.Fatal("clone shares panes")
	}
	// Equal compares instants: a time with a monotonic reading equals the
	// same instant without one.
	a := Record{Workstreams: []Workstream{{Name: "a", LastUsed: time.Now()}}}
	b := a.Clone()
	b.Workstreams[0].LastUsed = b.Workstreams[0].LastUsed.Round(0)
	if !a.Equal(b) {
		t.Fatal("equal records compare unequal")
	}
	b.Workstreams[0].Name = "z"
	if a.Equal(b) {
		t.Fatal("different records compare equal")
	}
}

func TestValidate(t *testing.T) {
	for name, ok := range map[string]bool{
		"infra": true, "work/infra": true, "my project": true,
		"": false, "  ": false, " infra": false, "v1.2": false, "a:b": false, "a\tb": false,
	} {
		if err := Validate(name); (err == nil) != ok {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func TestResumeTag(t *testing.T) {
	c, ok := ParseTag(EncodeTag(*convA))
	if !ok || *c != *convA {
		t.Fatalf("%+v", c)
	}
	for _, bad := range []string{"", "x", `{"agent":"claude"}`, `{"session_id":"x"}`} {
		if _, ok := ParseTag(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestPlan(t *testing.T) {
	w := Workstream{Name: "infra", Windows: []Window{
		{Index: 2, Name: "logs", AutoName: false, Layout: "abcd,160x40,0,0{80x40,0,0,1,79x40,81,0,2}", Active: false,
			Panes: []Pane{{Cwd: "/w/a", Kind: KindShell}, {Cwd: "/w/b", Kind: KindConversation, Active: true,
				Conv: Conversation{Agent: model.Claude, SessionID: "c1", ConfigDir: "/h/.claude", DefaultProfile: true, Title: "Fix it"}}}},
		{Index: 3, Name: "claude", AutoName: true, Layout: "ef01,160x40,0,0,3", Active: true,
			Panes: []Pane{{Cwd: "/w/c", Kind: KindShell}}},
	}}
	cmds := Plan(w, Bases{Window: 0, Pane: 1}, "/bin/cccp")
	var got []string
	for _, c := range cmds {
		s := strings.Join(c.Args, " ")
		if c.MarkResume != "" {
			s += "  [mark]"
		}
		got = append(got, s)
	}
	want := []string{
		"new-session -d -s infra -x 160 -y 40 -c /w/a -n logs",
		"move-window -s =infra:0 -t =infra:2",
		"split-window -d -t =infra:2.1 -P -F #{pane_id} -c /w/b /bin/cccp resume-pane --agent claude --id c1 --config-dir /h/.claude --default-profile --title Fix it  [mark]",
		"select-layout -t =infra:2 abcd,160x40,0,0{80x40,0,0,1,79x40,81,0,2}",
		"select-pane -t =infra:2.2",
		"new-window -d -t =infra:3 -c /w/c",
		"select-window -t =infra:3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if c, ok := ParseTag(cmds[2].MarkResume); !ok || c.SessionID != "c1" {
		t.Fatal("mark is the conversation")
	}
	// Panes are split from the last one made: a third pane splits pane 2.
	w.Windows[0].Panes = append(w.Windows[0].Panes, Pane{Cwd: "/w/d", Kind: KindShell})
	for _, c := range Plan(w, Bases{Window: 0, Pane: 0}, "/bin/cccp") {
		if c.Args[0] == "split-window" && c.Args[len(c.Args)-1] == "/w/d" && c.Args[3] != "=infra:2.1" {
			t.Fatalf("third pane split from %s", c.Args[3])
		}
	}
	if Plan(Workstream{Name: "x"}, Bases{}, "b") != nil {
		t.Fatal("nothing to restore")
	}
}

func TestLayoutSize(t *testing.T) {
	if x, y, ok := layoutSize("b25d,80x24,0,0,0"); !ok || x != 80 || y != 24 {
		t.Fatal(x, y, ok)
	}
	if _, _, ok := layoutSize("garbage"); ok {
		t.Fatal("garbage parsed")
	}
}

// Session ids repeat across servers ($1 on each): a pending mark is only
// ever about its own server's session.
func TestPendingIsOnlyAboutItsServer(t *testing.T) {
	rec := Record{
		Workstreams: []Workstream{{Name: "docs", ID: "$1", Server: srvB}},
		Pending:     []Pending{{Name: "infra", ID: "$1", Server: srvA, Since: t0}},
	}
	// docs ($1 on B) closes, long after A's mark: it gets its own grace.
	rec = Observe(rec, live(srvB, pane("other", "$2", 0, 0, "/w")), t0.Add(time.Hour))
	if _, ok := rec.Find("docs"); !ok || len(rec.Pending) != 1 || !rec.Pending[0].Since.Equal(t0.Add(time.Hour)) {
		t.Fatalf("forgotten on another server's mark: %+v", rec)
	}
}

// A server whose identity could not be read is never "the same server" as
// a recorded one, even another unidentified one: nothing is forgotten on
// the strength of two zeros.
func TestAnUnidentifiedServerForgetsNothing(t *testing.T) {
	rec := Record{Workstreams: []Workstream{{Name: "a", ID: "$1"}}} // recorded without an identity
	unknown := Live{OK: true, Running: true, Panes: []LivePane{pane("b", "$2", 0, 0, "/w")}}
	for _, at := range []time.Time{t0, t0.Add(time.Hour)} {
		rec = Observe(rec, unknown, at)
	}
	if a, ok := rec.Find("a"); !ok || !a.Dormant || len(rec.Pending) != 0 {
		t.Fatalf("%+v", rec)
	}
}

func TestPlanDoesNotMoveAWindowAlreadyInPlace(t *testing.T) {
	w := Workstream{Name: "x", Windows: []Window{{Index: 1, AutoName: true, Panes: []Pane{{Cwd: "/w", Kind: KindShell}}}}}
	for _, c := range Plan(w, Bases{Window: 1}, "b") {
		if c.Args[0] == "move-window" {
			t.Fatalf("moved a window already at its index: %v", c.Args)
		}
	}
}

func TestAnyPaneMarksItsWindowActive(t *testing.T) {
	first := pane("s", "$1", 0, 0, "/w")
	first.Pane.WindowActive = true
	rec := Observe(Record{}, live(srvA, first, pane("s", "$1", 0, 1, "/w")), t0)
	if w, _ := rec.Find("s"); !w.Windows[0].Active {
		t.Fatal("the active window was lost by a later pane")
	}
}
