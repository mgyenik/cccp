package profile

import (
	"reflect"
	"testing"

	"cccp/internal/config"
	"cccp/internal/model"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{"~/.claude-work": "/h/.claude-work", "/x//y/": "/x/y", "~": "/h", "": ""} {
		if got := Normalize(in, "/h"); got != want {
			t.Errorf("%q → %q want %q", in, got, want)
		}
	}
}

func TestResolveDiscoveredOnly(t *testing.T) {
	got := Resolve(nil, "/h/.claude", "/h", []string{"/h/.claude-work", "/h/.claude", "/h/.claude-work/"})
	want := []model.Profile{
		{Name: "default", Dir: "/h/.claude", Default: true},
		{Name: "work", Dir: "/h/.claude-work"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestResolveConfiguredNamesAndOverrides(t *testing.T) {
	got := Resolve([]config.Profile{
		{Name: "work", Dir: "~/.claude-work", Summaries: "off"},
		{Name: "personal", Dir: "~/.claude"},
	}, "/h/.claude", "/h", []string{"/srv/claude-ci"})
	want := []model.Profile{
		{Name: "personal", Dir: "/h/.claude", Default: true},
		{Name: "work", Dir: "/h/.claude-work", Summaries: "off"},
		{Name: "ci", Dir: "/srv/claude-ci"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestDerivedNamesAreUnique(t *testing.T) {
	got := Resolve([]config.Profile{{Name: "work", Dir: "/a/other"}}, "/h/.claude", "/h", []string{"/h/.claude-work"})
	if got[1].Name != "work" || got[2].Name != "work-2" {
		t.Fatalf("%+v", got)
	}
}

func TestFind(t *testing.T) {
	ps := Resolve(nil, "/h/.claude", "/h", []string{"/h/.claude-work"})
	if p, ok := Find(ps, "", "/h"); !ok || !p.Default {
		t.Fatal("empty dir is the default profile")
	}
	if p, ok := Find(ps, "~/.claude-work", "/h"); !ok || p.Name != "work" {
		t.Fatalf("%+v %v", p, ok)
	}
	if _, ok := Find(ps, "/elsewhere", "/h"); ok {
		t.Fatal("unknown dir found")
	}
}
