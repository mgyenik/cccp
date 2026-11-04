// Package dirpick ranks directories for the cockpit's directory picker (a
// new workstream's directory). Pure: the filesystem is reached through the
// List and Exists functions it is given.
//
// Two modes, by what is typed:
//
//   - a path (starting with "~" or "/"): the path itself if it exists, then
//     the children of its deepest directory, filtered by the last segment;
//     a path that does not exist gets an explicit "create" choice after
//     the matches (so Enter on a near miss opens the match rather than
//     creating a directory; with no match, create is the only choice);
//   - anything else: a fuzzy search over the directories cccp knows you use
//     (most recent first) and those found under $HOME.
//
// With nothing typed, $HOME comes first, then the known directories.
package dirpick

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cccp/internal/fuzzy"
)

// Known is a directory cccp has seen in use, and when.
type Known struct {
	Path string
	At   time.Time
}

// Choice is one row of the picker. Create: the directory does not exist and
// choosing it creates it.
type Choice struct {
	Path   string
	Create bool
}

// Source is what the picker draws from.
type Source struct {
	Home   string
	Known  []Known
	Walked []string // found under Home (bounded walk)
	// List returns the child directories of dir (names, not paths);
	// Exists reports whether path is a directory.
	List   func(dir string) []string
	Exists func(path string) bool
}

// Display shows a path with "~" for the home directory.
func Display(path, home string) string {
	if home != "" && (path == home || strings.HasPrefix(path, home+"/")) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}

// Expand turns a typed "~" path into an absolute one.
func Expand(q, home string) string {
	if q == "~" || strings.HasPrefix(q, "~/") {
		return home + strings.TrimPrefix(q, "~")
	}
	return q
}

// IsPath: the query is a path, not a fuzzy search.
func IsPath(q string) bool { return strings.HasPrefix(q, "~") || strings.HasPrefix(q, "/") }

// Choices returns at most limit choices for query, best first.
func Choices(query string, src Source, limit int) []Choice {
	q := strings.TrimSpace(query)
	var out []Choice
	switch {
	case q == "":
		out = append(out, Choice{Path: src.Home})
		for _, p := range candidates(src) {
			out = append(out, Choice{Path: p})
		}
	case IsPath(q):
		out = pathChoices(q, src)
	default:
		out = fuzzyChoices(q, src)
	}
	out = dedupe(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// candidates: known directories (most recent first, existing only), then
// the walked ones.
func candidates(src Source) []string {
	known := append([]Known(nil), src.Known...)
	sort.SliceStable(known, func(i, j int) bool { return known[i].At.After(known[j].At) })
	var out []string
	for _, k := range known {
		if k.Path != "" && src.Exists(k.Path) {
			out = append(out, filepath.Clean(k.Path))
		}
	}
	return append(out, src.Walked...)
}

func pathChoices(q string, src Source) []Choice {
	p := Expand(q, src.Home)
	clean := filepath.Clean(p)
	dir, prefix := clean, ""
	if !strings.HasSuffix(q, "/") && q != "~" {
		dir, prefix = filepath.Dir(clean), filepath.Base(clean)
	}
	exists := src.Exists(clean)
	var out []Choice
	if exists {
		out = append(out, Choice{Path: clean})
	}
	out = append(out, children(dir, prefix, src)...)
	if !exists {
		out = append(out, Choice{Path: clean, Create: true})
	}
	return out
}

// children are dir's subdirectories matching prefix (fuzzily), best first;
// hidden ones only when prefix asks for them.
func children(dir, prefix string, src Source) []Choice {
	if !src.Exists(dir) {
		return nil
	}
	type scored struct {
		name  string
		score int
	}
	var kids []scored
	for _, name := range src.List(dir) {
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		if s, ok := fuzzy.Match(prefix, name); ok {
			kids = append(kids, scored{name, s})
		}
	}
	sort.SliceStable(kids, func(i, j int) bool {
		if kids[i].score != kids[j].score {
			return kids[i].score > kids[j].score
		}
		return kids[i].name < kids[j].name
	})
	out := make([]Choice, 0, len(kids))
	for _, k := range kids {
		out = append(out, Choice{Path: filepath.Join(dir, k.name)})
	}
	return out
}

func fuzzyChoices(q string, src Source) []Choice {
	type scored struct {
		path  string
		score int
		order int // candidates' order: known (recent first), then walked
	}
	var hits []scored
	for i, p := range candidates(src) {
		if s, ok := fuzzy.Match(q, Display(p, src.Home)); ok {
			hits = append(hits, scored{p, s, i})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].order < hits[j].order
	})
	out := make([]Choice, 0, len(hits))
	for _, h := range hits {
		out = append(out, Choice{Path: h.path})
	}
	return out
}

func dedupe(cs []Choice) []Choice {
	seen := map[string]bool{}
	out := cs[:0:0]
	for _, c := range cs {
		if c.Path == "" || seen[c.Path] {
			continue
		}
		seen[c.Path] = true
		out = append(out, c)
	}
	return out
}
