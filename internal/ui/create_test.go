package ui

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"cccp/internal/core"
	"cccp/internal/workstream"
)

// fakeDirs is a fixed directory tree.
type fakeDirs struct {
	tree  map[string][]string // dir → child names
	walks int
}

func (f *fakeDirs) Walk(root string) []string {
	f.walks++
	return []string{root + "/git", root + "/git/infra", root + "/notes"}
}
func (f *fakeDirs) List(dir string) []string { return f.tree[dir] }
func (f *fakeDirs) Exists(p string) bool {
	if _, ok := f.tree[p]; ok {
		return true
	}
	for dir, kids := range f.tree {
		for _, k := range kids {
			if dir+"/"+k == p {
				return true
			}
		}
	}
	return false
}

func newFakeDirs() *fakeDirs {
	return &fakeDirs{tree: map[string][]string{"/h": {"git", "notes"}, "/h/git": {"infra", "cccp"}, "/h/notes": nil}}
}

func press(c *creator, keys ...string) (done bool, res *Created) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		default:
			for _, r := range k {
				c.update(tea.KeyPressMsg{Code: r, Text: string(r)})
			}
			continue
		}
		_, done, res = c.update(msg)
		if done {
			return
		}
	}
	return
}

// focused is a creator as the cockpit starts it: the name input focused.
func focused(d *fakeDirs) *creator {
	c := newCreator(wsSnap(), "/h", d)
	c.focus()
	return c
}

func wsSnap() core.Snapshot {
	return core.Snapshot{
		Sessions: []core.Session{{Key: "claude:a", Cwd: "/h/git/cccp"}},
		Workstreams: workstream.Record{Workstreams: []workstream.Workstream{
			{Name: "infra"}, {Name: "old", Dormant: true},
		}},
	}
}

func TestCreatorNewWorkstreamInAPickedDirectory(t *testing.T) {
	d := newFakeDirs()
	c := focused(d)
	if done, _ := press(c, "api", "enter"); done || c.step != stepDir {
		t.Fatalf("name accepted: step %v", c.step)
	}
	// Nothing typed: $HOME first, then the known directory.
	if len(c.choices) < 2 || c.choices[0].Path != "/h" || c.choices[1].Path != "/h/git/cccp" {
		t.Fatalf("%+v", c.choices)
	}
	c.walked(d.Walk("/h"))
	// Tab completes the selection into the input, to append to it.
	press(c, "~/git/inf", "tab")
	if v := c.dir.Value(); v != "~/git/infra/" {
		t.Fatalf("tab: %q (choices %+v)", v, c.choices)
	}
	done, res := press(c, "enter")
	if !done || res == nil || res.Name != "api" || res.Dir != "/h/git/infra" || res.Create {
		t.Fatalf("%v %+v", done, res)
	}
}

func TestCreatorCreatesATypedPath(t *testing.T) {
	c := focused(newFakeDirs())
	press(c, "new", "enter", "~/git/brand-new")
	if len(c.choices) == 0 || !c.choices[0].Create {
		t.Fatalf("%+v", c.choices)
	}
	if !strings.Contains(c.view(80, 20), "+ create ~/git/brand-new") {
		t.Fatal("the create row is shown")
	}
	_, res := press(c, "enter")
	if res == nil || res.Dir != "/h/git/brand-new" || !res.Create {
		t.Fatalf("%+v", res)
	}
}

func TestCreatorNames(t *testing.T) {
	c := focused(newFakeDirs())
	// An existing workstream (running or dormant) opens instead.
	if done, res := press(c, "old", "enter"); !done || res == nil || res.Open != "old" {
		t.Fatalf("%v %+v", done, res)
	}
	// An invalid name is refused in place.
	c = focused(newFakeDirs())
	if done, _ := press(c, "v1.2", "enter"); done || c.step != stepName || !strings.Contains(c.view(80, 10), "'.'") {
		t.Fatalf("done %v step %v view %q", done, c.step, c.view(80, 10))
	}
	// Esc: back from the directory to the name, then out.
	c = focused(newFakeDirs())
	press(c, "x", "enter")
	if done, _ := press(c, "esc"); done || c.step != stepName {
		t.Fatal("esc in the directory step goes back")
	}
	if done, res := press(c, "esc"); !done || res != nil {
		t.Fatal("esc in the name step cancels")
	}
}

func TestCreatorWalksOnce(t *testing.T) {
	d := newFakeDirs()
	c := focused(d)
	press(c, "x")
	cmd, _, _ := c.update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !c.walking || cmd == nil {
		t.Fatal("the walk starts with the directory step")
	}
	c.walked(nil) // found nothing
	press(c, "esc")
	c.update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if c.walking {
		t.Fatal("walked again after coming back")
	}
	if got := c.src.Walked; got == nil || len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestCockpitWorkstreamActions(t *testing.T) {
	m, r, _ := newModel(t)
	var opened, made []string
	m.opt.Actions.OpenWorkstream = func(n string) error { opened = append(opened, n); return nil }
	m.opt.Actions.NewWorkstream = func(n, d string, c bool) error {
		made = append(made, n+"|"+d+"|"+map[bool]string{true: "create", false: "existing"}[c])
		return nil
	}
	m.opt.Home, m.opt.Dirs = "/h", newFakeDirs()
	snap := snapshot()
	snap.Workstreams = wsSnap().Workstreams
	m.Update(snapMsg(snap))
	key(m, "w")
	if m.mode != modeCreate || !strings.Contains(m.render(), "New workstream") {
		t.Fatal("w opens the flow")
	}
	typeText(m, "api")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // the name; its focus and walk commands are not run here
	m.Update(walkedMsg{dirs: []string{"/h/git"}})
	typeText(m, "~/fresh")
	key(m, "enter")
	if !reflect.DeepEqual(made, []string{"api|/h/fresh|create"}) || !m.quitting {
		t.Fatalf("made %v quitting %v", made, m.quitting)
	}
	// An existing name opens it.
	m, _, _ = newModel(t)
	m.opt.Actions.OpenWorkstream = func(n string) error { opened = append(opened, n); return nil }
	m.opt.Actions.NewWorkstream = func(string, string, bool) error { return nil }
	m.Update(snapMsg(snap))
	key(m, "w")
	typeText(m, "infra")
	key(m, "enter")
	if !reflect.DeepEqual(opened, []string{"infra"}) || !m.quitting {
		t.Fatalf("opened %v", opened)
	}
	_ = r
}

func TestHeaderShowsTheCurrentWorkstream(t *testing.T) {
	m, _, _ := newModel(t)
	header := func() string { return ansi.Strip(strings.Split(m.render(), "\n")[0]) }
	if strings.Contains(header(), "[") {
		t.Fatalf("no workstream known: none shown: %q", header())
	}
	m.opt.Workstream = "infra"
	if !strings.HasPrefix(header(), "cccp [infra]") {
		t.Fatal("current workstream not in the header")
	}
}
