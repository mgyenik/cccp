package jail

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Property is one unit property as systemd's D-Bus API takes it. Value is
// a uint64 (D-Bus "t"), a uint32 ("u") for the …Scale properties, or a
// string ("s").
type Property struct {
	Name  string
	Value any
}

// limitMax is systemd's "no limit" (CGROUP_LIMIT_MAX).
const limitMax = math.MaxUint64

// Properties encodes limits for SetUnitProperties / StartTransientUnit the
// way systemctl does (systemd v255, src/shared/bus-unit-util.c): a
// percentage goes in NAMEScale as a fraction of UINT32_MAX (systemd works
// out the bytes from the machine's memory), a size in NAME as bytes (base
// 1024 suffixes), "infinity" as no limit. Empty fields are left out.
func Properties(l Limits) ([]Property, error) {
	var out []Property
	for _, f := range []struct {
		name, value string
	}{{"MemoryHigh", l.MemoryHigh}, {"MemoryMax", l.MemoryMax}, {"MemorySwapMax", l.MemorySwapMax}} {
		if f.value == "" {
			continue
		}
		p, err := memoryProperty(f.name, f.value)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if l.CPUWeight != 0 {
		if l.CPUWeight < 1 || l.CPUWeight > 10000 {
			return nil, fmt.Errorf("cpu_weight %d: systemd takes 1 to 10000", l.CPUWeight)
		}
		out = append(out, Property{"CPUWeight", uint64(l.CPUWeight)})
	}
	if l.TasksMax != 0 {
		if l.TasksMax < 0 {
			return nil, fmt.Errorf("tasks_max %d", l.TasksMax)
		}
		out = append(out, Property{"TasksMax", uint64(l.TasksMax)})
	}
	return out, nil
}

// ScopeProperties are set on every scope cccp makes (the tmux server's and
// each pane's). OOMPolicy continue: when the kernel kills a process at a
// workstream's limit, systemd leaves the rest of the pane alone. Its default
// (stop) would then stop the whole scope: SIGTERM to the agent and shell in
// the pane, SIGKILL 90 s later, and the unit left failed (measured
// 2026-11-05, docs/jails.md).
func ScopeProperties() []Property {
	return []Property{{"OOMPolicy", "continue"}}
}

func memoryProperty(name, v string) (Property, error) {
	v = strings.TrimSpace(v)
	if v == "infinity" {
		return Property{name, uint64(limitMax)}, nil
	}
	if strings.HasSuffix(v, "%") {
		p, err := permyriad(strings.TrimSuffix(v, "%"))
		if err != nil {
			return Property{}, fmt.Errorf("%s %q: %v", name, v, err)
		}
		return Property{name + "Scale", scaleFromPermyriad(p)}, nil
	}
	n, err := ParseSize(v)
	if err != nil {
		return Property{}, fmt.Errorf("%s %q: %v", name, v, err)
	}
	return Property{name, n}, nil
}

// permyriad parses a percentage's number ("70", "70.5") into hundredths of
// a percent, at most two decimals, 0 to 100%.
func permyriad(s string) (int, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 {
		return 0, fmt.Errorf("at most two decimals")
	}
	w, err := strconv.Atoi(whole)
	if err != nil {
		return 0, fmt.Errorf("not a percentage")
	}
	f := 0
	if frac != "" {
		if f, err = strconv.Atoi(frac + strings.Repeat("0", 2-len(frac))); err != nil {
			return 0, fmt.Errorf("not a percentage")
		}
	}
	p := w*100 + f
	if w < 0 || p > 10000 {
		return 0, fmt.Errorf("out of 0–100%%")
	}
	return p, nil
}

// scaleFromPermyriad is systemd's UINT32_SCALE_FROM_PERMYRIAD.
func scaleFromPermyriad(p int) uint32 {
	return uint32((uint64(p)*math.MaxUint32 + 5000) / 10000)
}

// ParseSize parses a size as systemd does for memory limits: a number with
// an optional K, M, G, T, P or E suffix (base 1024), fractions allowed
// ("1.5G").
func ParseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	mult := uint64(1)
	if n := len(s); n > 0 {
		if i := strings.IndexByte("KMGTPE", s[n-1]); i >= 0 {
			mult = uint64(1) << (10 * (i + 1))
			s = s[:n-1]
		} else if s[n-1] == 'B' {
			s = s[:n-1]
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || strings.ContainsAny(s, "eE+-") {
		return 0, fmt.Errorf("not a size")
	}
	v := f * float64(mult)
	if v >= math.MaxUint64 {
		return 0, fmt.Errorf("too large")
	}
	return uint64(v), nil
}

// LimitBytes is a memory limit in bytes on a machine with total bytes of
// memory: a percentage of it, or a size (0: none, or not understood).
func LimitBytes(limit string, total int64) int64 {
	limit = strings.TrimSpace(limit)
	switch {
	case limit == "" || limit == "infinity":
		return 0
	case strings.HasSuffix(limit, "%"):
		p, err := permyriad(strings.TrimSuffix(limit, "%"))
		if err != nil {
			return 0
		}
		return total * int64(p) / 10000
	}
	n, err := ParseSize(limit)
	if err != nil {
		return 0
	}
	return int64(n)
}
