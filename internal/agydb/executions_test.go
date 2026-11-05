package agydb

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"cccp/internal/model"
)

// The leading fields of real rows (agy 1.4.12 / 1.5.1): reason 2, 57 model
// calls, last step 690, then a string field (9).
var canceledRow = []byte{0x08, 0x02, 0x10, 0x39, 0x18, 0xb2, 0x05, 0x4a, 0x03, 'a', 'b', 'c'}

func TestDecodeExecution(t *testing.T) {
	if got := DecodeExecution(canceledRow); got != (model.AgyExecution{Known: true, Reason: model.AgyUserCanceled, LastStep: 690}) {
		t.Fatalf("%+v", got)
	}
	normal := []byte{0x08, 0x04, 0x10, 0x02, 0x18, 0x08}
	if got := DecodeExecution(normal); got.Reason != model.AgyNoToolCall || got.LastStep != 8 {
		t.Fatalf("%+v", got)
	}
	for name, b := range map[string][]byte{
		"empty":            nil,
		"no last step":     {0x08, 0x02, 0x10, 0x01, 0x4a, 0x01, 'x'},
		"no reason":        {0x10, 0x01, 0x18, 0x05},
		"reason 0":         {0x08, 0x00, 0x18, 0x05},
		"truncated varint": {0x08, 0x02, 0x18, 0xb2},
		"string first":     {0x0a, 0x01, 'x', 0x08, 0x02, 0x18, 0x05},
		"string, as if":    {0x0a, 0x02, 0x18, 0x05}, // read as varints: reason 2, step 5
	} {
		if got := DecodeExecution(b); got.Known {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestExecutionsReadsTheLatestRow(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c1.db")
	sub := filepath.Join(dir, "conversations")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: p}).EscapedPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE `executor_metadata` (`idx` integer,`data` blob,PRIMARY KEY (`idx`))"); err != nil {
		t.Fatal(err)
	}
	x := NewExecutions(dir)
	if got := x.Last("c1"); got.Known {
		t.Fatalf("no executions yet: %+v", got)
	}
	for i, row := range [][]byte{{0x08, 0x04, 0x10, 0x01, 0x18, 0x03}, canceledRow} {
		if _, err := db.Exec("INSERT INTO executor_metadata VALUES (?, ?)", i, row); err != nil {
			t.Fatal(err)
		}
	}
	if got := x.Last("c1"); got.Reason != model.AgyUserCanceled || got.LastStep != 690 {
		t.Fatalf("latest: %+v", got)
	}
	for _, id := range []string{"missing", ""} {
		if got := x.Last(id); got.Known {
			t.Errorf("%q: %+v", id, got)
		}
	}
	// An id names a file in the directory, never one outside it.
	if got := NewExecutions(sub).Last("../c1"); got.Known {
		t.Fatalf("escaped the directory: %+v", got)
	}
}
