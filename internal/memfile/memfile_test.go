package memfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"cccp/internal/core"
)

func TestRoundTrip(t *testing.T) {
	f := File{Path: filepath.Join(t.TempDir(), "d", "daemon.json")}
	m, err := f.Load()
	if err != nil || len(m.Seen) != 0 || m.Seen == nil {
		t.Fatalf("missing file: %v %+v", err, m)
	}
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m.Seen["k"] = at
	m.Aliases["k"] = "box"
	m.Summaries["k"] = core.SummaryRecord{InputHash: "h", Text: "t", At: at}
	m.Initialized = true
	m.RegLast["k"] = core.RegStatus{Status: "busy"} // not persisted
	if err := f.Save(m); err != nil {
		t.Fatal(err)
	}
	got, err := f.Load()
	if err != nil || !got.Persisted(m) || len(got.RegLast) != 0 {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestCorruptFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "daemon.json")
	os.WriteFile(p, []byte("{not json"), 0o600)
	m, err := File{Path: p}.Load()
	if err == nil || m.Seen == nil {
		t.Fatalf("want an error and usable empty memory: %v %+v", err, m)
	}
}
