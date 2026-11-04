// Package dirs reads directories for the cockpit's directory picker
// (dirpick): a bounded walk under a root, a directory's children, and
// whether a path is a directory.
package dirs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Walk limits.
const (
	MaxDepth = 4
	MaxDirs  = 20000
)

// Skip are directory names the walk does not enter: dependency and build
// trees, which hold many directories nobody opens a workstream in. Hidden
// directories (".git", ".cache", …) are skipped too.
var Skip = map[string]bool{
	"node_modules": true, "target": true, "vendor": true, "__pycache__": true,
	"venv": true, "dist": true, "build": true,
}

// SkipTop are skipped only directly under the root: in a home directory,
// Go's module cache (~/go) and snap's data (~/snap), each tens of thousands
// of directories; a project named "go" deeper down is kept.
var SkipTop = map[string]bool{"go": true, "snap": true}

// FS is the real filesystem.
type FS struct{}

// Walk returns directories under root, breadth first (shallow ones first),
// to MaxDepth levels and at most MaxDirs of them. Symlinks are not followed
// (no loops); unreadable directories are passed over.
func (FS) Walk(root string) []string {
	var out []string
	level := []string{root}
	for depth := 1; depth <= MaxDepth && len(level) > 0; depth++ {
		var next []string
		for _, dir := range level {
			ents, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				name := e.Name()
				if !e.IsDir() || strings.HasPrefix(name, ".") || Skip[name] || depth == 1 && SkipTop[name] {
					continue
				}
				p := filepath.Join(dir, name)
				out = append(out, p)
				if len(out) >= MaxDirs {
					return out
				}
				next = append(next, p)
			}
		}
		level = next
	}
	return out
}

// List returns the names of dir's child directories, hidden ones included,
// and symlinks to directories.
func (FS) List(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		} else if e.Type()&os.ModeSymlink != 0 {
			if fi, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && fi.IsDir() {
				out = append(out, e.Name())
			}
		}
	}
	sort.Strings(out)
	return out
}

// Exists reports whether path is a directory.
func (FS) Exists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
