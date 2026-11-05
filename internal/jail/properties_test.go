package jail

import (
	"reflect"
	"testing"
)

func TestPropertiesEncodeAsSystemctlDoes(t *testing.T) {
	got, err := Properties(Limits{MemoryHigh: "70%", MemoryMax: "80.5%", MemorySwapMax: "2G", CPUWeight: 50, TasksMax: 4096})
	if err != nil {
		t.Fatal(err)
	}
	// (permyriad × UINT32_MAX + 5000) / 10000, as UINT32_SCALE_FROM_PERMYRIAD.
	want := []Property{
		{"MemoryHighScale", uint32(3006477107)},
		{"MemoryMaxScale", uint32(3457448672)},
		{"MemorySwapMax", uint64(2 << 30)},
		{"CPUWeight", uint64(50)},
		{"TasksMax", uint64(4096)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
	if p, _ := Properties(Limits{MemoryMax: "infinity"}); p[0] != (Property{"MemoryMax", uint64(limitMax)}) {
		t.Fatalf("%#v", p)
	}
	if p, _ := Properties(Limits{}); len(p) != 0 {
		t.Fatal("empty limits set nothing")
	}
	for _, bad := range []Limits{{MemoryHigh: "120%"}, {MemoryHigh: "lots"}, {MemoryMax: "1.234%"}, {CPUWeight: 20000}, {MemorySwapMax: "-1G"}} {
		if _, err := Properties(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	// Every built-in preset encodes.
	for _, l := range append([]Limits{Defaults.Root, Defaults.RootMax}, Defaults.Presets[PresetNormal], Defaults.Presets[PresetBoost], Defaults.Presets[PresetMax]) {
		if _, err := Properties(l); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"512": 512, "1K": 1024, "1.5G": 3 << 29, "2T": 2 << 40, "100B": 100} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("%q: %d %v", in, got, err)
		}
	}
}

func TestLimitBytes(t *testing.T) {
	total := int64(86 << 30)
	for in, want := range map[string]int64{"50%": 43 << 30, "37.5%": total * 3750 / 10000, "8G": 8 << 30, "": 0, "infinity": 0, "lots": 0} {
		if got := LimitBytes(in, total); got != want {
			t.Errorf("%q: %d want %d", in, got, want)
		}
	}
}

// A kill at the limit must not make systemd stop the rest of the pane.
func TestScopesContinueAfterAKill(t *testing.T) {
	ps := ScopeProperties()
	if len(ps) != 1 || ps[0].Name != "OOMPolicy" || ps[0].Value != "continue" {
		t.Fatalf("%+v", ps)
	}
}
