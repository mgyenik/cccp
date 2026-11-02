package procs

import (
	"os"
	"testing"
)

func TestParseStatCommWithSpacesAndParens(t *testing.T) {
	line := "1234 (my (weird) proc) S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 98765 1000 10 18446744073709551615"
	st, err := parseStat(1234, []byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if st.Comm != "my (weird) proc" || st.State != 'S' || st.PPID != 1 || st.StartTime != 98765 {
		t.Fatalf("%+v", st)
	}
}

func TestAliveSelf(t *testing.T) {
	st, err := ReadStat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !Alive(st.PID, st.StartTime) {
		t.Fatal("self not alive")
	}
	if Alive(st.PID, st.StartTime+1) {
		t.Fatal("start time mismatch must mean a different process")
	}
}
