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
