package config

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	c, err := Parse([]byte("notify = false\nsummaries = \"always\"\ndefault_cache_ttl = \"5m\"\n"))
	if err != nil || c.Notify || c.Summaries != "always" || c.DefaultCacheTTL.Duration != 5*time.Minute {
		t.Fatalf("%v %+v", err, c)
	}
	if c.EndedRetention != Default().EndedRetention {
		t.Fatal("unset fields must keep their defaults")
	}
	for _, bad := range []string{"summaries = \"sometimes\"", "quiet_after = \"soon\"", "notify = "} {
		if c, err := Parse([]byte(bad)); err == nil || c.Notify != Default().Notify {
			t.Errorf("%q: want an error and the defaults", bad)
		}
	}
	if c, err := Parse(nil); err != nil || c.Summaries != Default().Summaries {
		t.Fatal("empty input is the defaults")
	}
}

func TestParseProfiles(t *testing.T) {
	c, err := Parse([]byte("[[profile]]\nname = \"work\"\ndir = \"~/.claude-work\"\nsummaries = \"off\"\n"))
	if err != nil || len(c.Profiles) != 1 || c.Profiles[0] != (Profile{Name: "work", Dir: "~/.claude-work", Summaries: "off"}) {
		t.Fatalf("%v %+v", err, c.Profiles)
	}
	for _, bad := range []string{
		"[[profile]]\nname = \"x\"\n",                                                        // no dir
		"[[profile]]\ndir = \"/a\"\nsummaries = \"maybe\"\n",                                 // bad policy
		"[[profile]]\nname = \"w\"\ndir = \"/a\"\n[[profile]]\nname = \"w\"\ndir = \"/b\"\n", // duplicate name
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
