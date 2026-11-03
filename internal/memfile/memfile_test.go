package memfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cccp/internal/core"
)

func files(t *testing.T) File {
	d := t.TempDir()
	return File{Path: filepath.Join(d, "state", "daemon.json"), SummariesPath: filepath.Join(d, "state", "summaries.json")}
}

func TestRoundTripInTwoFiles(t *testing.T) {
	f := files(t)
	m, err := f.Load()
	if err != nil || len(m.Seen) != 0 || m.Seen == nil || m.Summaries == nil {
		t.Fatalf("missing files: %v %+v", err, m)
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
	daemon, _ := os.ReadFile(f.Path)
	if strings.Contains(string(daemon), "input_hash") {
		t.Fatal("summaries written into daemon.json")
	}
	sums, _ := os.ReadFile(f.SummariesPath)
	if !strings.Contains(string(sums), `"input_hash": "h"`) {
		t.Fatalf("summaries.json: %s", sums)
	}
	got, err := f.Load()
	if err != nil || !got.Persisted(m) || len(got.RegLast) != 0 {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestLegacySummariesAreMigrated(t *testing.T) {
	f := files(t)
	os.MkdirAll(filepath.Dir(f.Path), 0o700)
	os.WriteFile(f.Path, []byte(`{"seen":{},"aliases":{"k":"box"},"reg_completions":{},"initialized":true,
		"summaries":{"k":{"input_hash":"old","text":"from daemon.json","at":"2026-11-01T00:00:00Z"}}}`), 0o600)
	m, err := f.Load()
	if err != nil || m.Summaries["k"].Text != "from daemon.json" || m.Aliases["k"] != "box" {
		t.Fatalf("%v %+v", err, m)
	}
	f.Save(m)
	m2, _ := f.Load()
	if m2.Summaries["k"].Text != "from daemon.json" {
		t.Fatal("migrated summary lost")
	}
}

func TestCorruptFileStillLoadsTheOther(t *testing.T) {
	f := files(t)
	os.MkdirAll(filepath.Dir(f.Path), 0o700)
	os.WriteFile(f.Path, []byte("{not json"), 0o600)
	os.WriteFile(f.SummariesPath, []byte(`{"k":{"input_hash":"h","text":"kept"}}`), 0o600)
	m, err := f.Load()
	if err == nil || m.Seen == nil || m.Summaries["k"].Text != "kept" {
		t.Fatalf("want an error, usable memory and the summaries: %v %+v", err, m)
	}
}
