// Package archtest enforces the architecture: the functional-core packages
// perform no I/O and depend on no adapter.
package archtest

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// pure lists the functional-core packages and the cccp packages each may
// import.
var pure = map[string][]string{
	"model":      {},
	"reduce":     {"model"},
	"config":     {"jail"},
	"core":       {"model", "config", "workstream", "jail"},
	"present":    {"core", "model"},
	"machine":    {"core", "model", "config", "present", "jail"},
	"profile":    {"config", "model"},
	"fuzzy":      {},
	"nav":        {},
	"workstream": {"model"},
	"dirpick":    {"fuzzy"},
	"jail":       {},
	"usage":      {},
}

// io are standard-library packages that perform I/O or reach the process
// environment; pure packages must not import them.
var io = []string{"os", "os/exec", "os/signal", "os/user", "net", "net/http", "syscall", "io/fs", "io/ioutil", "log", "log/slog", "plugin", "unsafe", "testing"}

// thirdParty lists the non-standard imports each pure package may use.
var thirdParty = map[string][]string{
	"config": {"github.com/BurntSushi/toml"},
}

func TestPurePackagesImportNoIO(t *testing.T) {
	root := filepath.Join("..")
	for pkg, allowed := range pure {
		files, err := filepath.Glob(filepath.Join(root, pkg, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: no files (%v)", pkg, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue // tests may use the testing package and temp files
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			af, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range af.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				switch {
				case strings.HasPrefix(path, "cccp/internal/"):
					name := strings.TrimPrefix(path, "cccp/internal/")
					if !contains(allowed, name) {
						t.Errorf("%s imports %s; pure packages may import only %v", f, path, allowed)
					}
				case path == "cccp":
					t.Errorf("%s imports the root package", f)
				case strings.Contains(strings.SplitN(path, "/", 2)[0], "."):
					if !contains(thirdParty[pkg], path) {
						t.Errorf("%s imports %s (third party)", f, path)
					}
				case contains(io, path):
					t.Errorf("%s imports %s, which performs I/O", f, path)
				}
			}
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
