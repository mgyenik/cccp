package dirpick

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

// tree is a fake filesystem: every directory, by absolute path.
type tree map[string]bool

func (t tree) list(dir string) []string {
	var out []string
	for p := range t {
		if p != dir && len(p) > len(dir) && p[:len(dir)] == dir && (dir == "/" || p[len(dir)] == '/') {
			rest := p[len(dir)+1:]
			if dir == "/" {
				rest = p[1:]
			}
			if rest != "" && !containsSlash(rest) {
				out = append(out, rest)
			}
		}
	}
	sort.Strings(out)
	return out
}

func containsSlash(s string) bool {
	for _, r := range s {
		if r == '/' {
			return true
		}
	}
	return false
}

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func source() Source {
	fs := tree{"/h": true, "/h/git": true, "/h/git/cccp": true, "/h/git/infra": true, "/h/git/.cache": true,
		"/h/notes": true, "/w": true, "/w/scratch": true}
	return Source{
		Home: "/h",
		Known: []Known{
			{Path: "/h/git/infra", At: t0},
			{Path: "/h/git/cccp", At: t0.Add(time.Hour)},
			{Path: "/h/gone", At: t0.Add(2 * time.Hour)}, // deleted since
		},
		Walked: []string{"/h/git", "/h/notes"},
		List:   fs.list,
		Exists: func(p string) bool { return fs[p] },
	}
}

func paths(cs []Choice) []string {
	var out []string
	for _, c := range cs {
		s := c.Path
		if c.Create {
			s = "+" + s
		}
		out = append(out, s)
	}
	return out
}

func TestNothingTyped(t *testing.T) {
	got := paths(Choices("", source(), 10))
	want := []string{"/h", "/h/git/cccp", "/h/git/infra", "/h/git", "/h/notes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v (home, then known by recency, existing only, then walked)", got, want)
	}
}

func TestFuzzyPrefersBetterMatchesThenKnown(t *testing.T) {
	if got := paths(Choices("infra", source(), 10)); len(got) == 0 || got[0] != "/h/git/infra" {
		t.Fatalf("%v", got)
	}
	// Both match equally: the known one comes first.
	if got := paths(Choices("git", source(), 10)); len(got) < 3 || got[0] != "/h/git" && got[0] != "/h/git/cccp" {
		t.Fatalf("%v", got)
	}
	if got := Choices("zzz", source(), 10); len(got) != 0 {
		t.Fatalf("no create row in fuzzy mode: %v", got)
	}
}

func TestPathMode(t *testing.T) {
	// A directory with a trailing slash: itself, then its children
	// (hidden ones left out).
	if got, want := paths(Choices("~/git/", source(), 10)), []string{"/h/git", "/h/git/cccp", "/h/git/infra"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// A partial last segment filters the siblings; the typed path does not
	// exist, so creating it is offered, after the matches (Enter on a near
	// miss opens the match).
	if got, want := paths(Choices("~/git/inf", source(), 10)), []string{"/h/git/infra", "+/h/git/inf"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// An existing path: itself first, no create row.
	if got := Choices("~/git/infra", source(), 10); len(got) == 0 || got[0].Create || got[0].Path != "/h/git/infra" {
		t.Fatalf("%v", got)
	}
	// Hidden children when asked for.
	if got := paths(Choices("~/git/.c", source(), 10)); !reflect.DeepEqual(got, []string{"/h/git/.cache", "+/h/git/.c"}) {
		t.Fatalf("%v", got)
	}
	// A new project several levels under a missing directory: just create.
	if got := paths(Choices("~/new/deep/proj", source(), 10)); !reflect.DeepEqual(got, []string{"+/h/new/deep/proj"}) {
		t.Fatalf("%v", got)
	}
	// Absolute paths, and "~" alone.
	if got := paths(Choices("/w/", source(), 10)); !reflect.DeepEqual(got, []string{"/w", "/w/scratch"}) {
		t.Fatalf("%v", got)
	}
	if got := Choices("~", source(), 10); len(got) == 0 || got[0].Path != "/h" || got[0].Create {
		t.Fatalf("%v", got)
	}
}

func TestLimitAndDisplay(t *testing.T) {
	if got := Choices("", source(), 2); len(got) != 2 {
		t.Fatalf("%v", got)
	}
	if Display("/h/git", "/h") != "~/git" || Display("/h", "/h") != "~" || Display("/hx", "/h") != "/hx" {
		t.Fatal("display")
	}
	if Expand("~/x", "/h") != "/h/x" || Expand("~", "/h") != "/h" || Expand("/a", "/h") != "/a" {
		t.Fatal("expand")
	}
}

func TestEqualMatchesPutKnownFirst(t *testing.T) {
	src := source()
	src.Known = []Known{{Path: "/h/x/infra", At: t0}}
	src.Walked = []string{"/h/y/infra"}
	exists := src.Exists
	src.Exists = func(p string) bool { return p == "/h/x/infra" || exists(p) }
	if got := paths(Choices("infra", src, 10)); len(got) < 2 || got[0] != "/h/x/infra" || got[1] != "/h/y/infra" {
		t.Fatalf("%v", got)
	}
}
