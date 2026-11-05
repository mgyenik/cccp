package config

import (
	"cccp/internal/jail"
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

func TestJailSection(t *testing.T) {
	if !Default().Jail.Enabled {
		t.Fatal("jails are on by default")
	}
	c, err := Parse([]byte(`
[jail]
root = { memory_high = "60%" }

[jail.preset.boost]
cpu_weight = 800

[[jail.workstream]]
name = "builds"
preset = "boost"

[[jail.workstream]]
name = "tight"
memory_max = "8G"
`))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Jail.Settings()
	if s.Root.MemoryHigh != "60%" || s.Root.MemoryMax != jail.Defaults.Root.MemoryMax {
		t.Fatalf("root merged over the defaults: %+v", s.Root)
	}
	if b := s.Presets[jail.PresetBoost]; b.CPUWeight != 800 || b.MemoryHigh != jail.Defaults.Presets[jail.PresetBoost].MemoryHigh {
		t.Fatalf("boost preset: %+v", b)
	}
	if s.Workstreams["builds"].Preset != jail.PresetBoost || s.Workstreams["tight"].Custom.MemoryMax != "8G" {
		t.Fatalf("workstreams: %+v", s.Workstreams)
	}
	if c, err := Parse([]byte("[jail]\nenabled = false\n")); err != nil || c.Jail.Enabled {
		t.Fatal("jails can be turned off")
	}
	for _, bad := range []string{
		"[jail.preset.turbo]\ncpu_weight = 1\n",
		"[jail]\nroot = { memory_high = \"lots\" }\n",
		"[[jail.workstream]]\npreset = \"boost\"\n",
		"[[jail.workstream]]\nname = \"x\"\npreset = \"turbo\"\n",
		"[[jail.workstream]]\nname = \"x\"\n[[jail.workstream]]\nname = \"x\"\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}
