package store

import (
	"sync"
	"testing"

	"cccp/internal/model"
)

func TestConcurrentUpdatesAreSerialised(t *testing.T) {
	st := Store{Dir: t.TempDir()}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.Update(model.Claude, "s", func(s model.HookState) model.HookState {
				s.ToolCalls++
				return s
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	s, err := st.Load(st.Path(model.Claude, "s"))
	if err != nil {
		t.Fatal(err)
	}
	if s.ToolCalls != 50 {
		t.Fatalf("lost updates: %d of 50", s.ToolCalls)
	}
}

func TestFileNameSanitised(t *testing.T) {
	if got := FileName(model.Agy, "../../etc/x"); got != "agy-______etc_x.json" {
		t.Fatalf("%q", got)
	}
}
