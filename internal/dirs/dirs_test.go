package dirs

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mk(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func rel(root string, ps []string) []string {
	var out []string
	for _, p := range ps {
		out = append(out, strings.TrimPrefix(p, root+"/"))
	}
	return out
}

func TestWalk(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "a/b/c/d/e", "proj/node_modules/x", "proj/src", ".hidden/x", "z", "go/pkg", "z/go")
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	got := rel(root, FS{}.Walk(root))
	// Breadth first, depth 4; ~/go skipped, z/go kept.
	want := []string{"a", "proj", "z", "a/b", "proj/src", "z/go", "a/b/c", "a/b/c/d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestListAndExists(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "b", "a", ".h")
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if got := (FS{}).List(root); !reflect.DeepEqual(got, []string{".h", "a", "b", "link"}) {
		t.Fatalf("%v", got)
	}
	if !(FS{}).Exists(root) || (FS{}).Exists(filepath.Join(root, "file")) || (FS{}).Exists(filepath.Join(root, "none")) {
		t.Fatal("exists")
	}
}
