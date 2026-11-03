// Package profile resolves the set of Claude profiles (config directories)
// from configuration and discovery. It is pure: discovery results are
// passed in.
package profile

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"cccp/internal/config"
	"cccp/internal/model"
)

// Normalize expands a leading "~/" and cleans dir. An empty dir stays empty.
func Normalize(dir, home string) string {
	if dir == "" {
		return ""
	}
	if dir == "~" {
		dir = home
	} else if strings.HasPrefix(dir, "~/") {
		dir = filepath.Join(home, dir[2:])
	}
	return filepath.Clean(dir)
}

// Resolve builds the profile list: the default profile first, then
// configured profiles in configuration order, then discovered ones sorted by
// directory. defaultDir is where Claude Code keeps its configuration when
// CLAUDE_CONFIG_DIR is unset (~/.claude). Directories are de-duplicated after
// normalisation; configuration supplies names and overrides.
func Resolve(configured []config.Profile, defaultDir, home string, discovered []string) []model.Profile {
	defaultDir = Normalize(defaultDir, home)
	byDir := map[string]*model.Profile{}
	var order []string
	add := func(dir string) *model.Profile {
		if p, ok := byDir[dir]; ok {
			return p
		}
		p := &model.Profile{Dir: dir, Default: dir == defaultDir}
		byDir[dir] = p
		order = append(order, dir)
		return p
	}
	add(defaultDir)
	for _, c := range configured {
		p := add(Normalize(c.Dir, home))
		if c.Name != "" {
			p.Name = c.Name
		}
		p.Summaries = c.Summaries
	}
	var disc []string
	for _, d := range discovered {
		if d = Normalize(d, home); d != "" {
			if _, ok := byDir[d]; !ok {
				disc = append(disc, d)
			}
		}
	}
	sort.Strings(disc)
	for _, d := range disc {
		add(d)
	}
	used := map[string]bool{}
	for _, dir := range order {
		if n := byDir[dir].Name; n != "" {
			used[n] = true
		}
	}
	out := make([]model.Profile, 0, len(order))
	for _, dir := range order {
		p := *byDir[dir]
		if p.Name == "" {
			p.Name = unique(derivedName(p), used)
			used[p.Name] = true
		}
		out = append(out, p)
	}
	return out
}

// derivedName names an unconfigured profile from its directory:
// ~/.claude → "default", ~/.claude-work → "work", /x/work-claude → "work-claude".
func derivedName(p model.Profile) string {
	if p.Default {
		return "default"
	}
	base := strings.TrimPrefix(filepath.Base(p.Dir), ".")
	for _, pre := range []string{"claude-", "claude_", "claude."} {
		if strings.HasPrefix(base, pre) && len(base) > len(pre) {
			return base[len(pre):]
		}
	}
	return base
}

func unique(name string, used map[string]bool) string {
	if !used[name] {
		return name
	}
	for i := 2; ; i++ {
		if n := name + "-" + strconv.Itoa(i); !used[n] {
			return n
		}
	}
}

// Find returns the profile for a session's config directory; "" means the
// default profile.
func Find(ps []model.Profile, dir, home string) (model.Profile, bool) {
	dir = Normalize(dir, home)
	for _, p := range ps {
		if (dir == "" && p.Default) || (dir != "" && p.Dir == dir) {
			return p, true
		}
	}
	return model.Profile{}, false
}
