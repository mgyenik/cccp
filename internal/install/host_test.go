package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetCgroupParent(t *testing.T) {
	got, err := SetCgroupParent(nil, ContainerSlice)
	if err != nil || string(got) != "{\n  \"cgroup-parent\": \"agent-containers.slice\"\n}\n" {
		t.Fatalf("%q %v", got, err)
	}
	// Other settings, nested ones too, are kept.
	cur := []byte(`{"log-driver": "journald", "default-ulimits": {"nofile": {"Name": "nofile", "Soft": 1024, "Hard": 4096}}}`)
	got, err = SetCgroupParent(cur, ContainerSlice)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil || m["log-driver"] != "journald" || m["cgroup-parent"] != ContainerSlice ||
		m["default-ulimits"].(map[string]any)["nofile"].(map[string]any)["Hard"] != 4096.0 {
		t.Fatalf("%s %v", got, err)
	}
	// Already ours: unchanged. Someone else's: refused. Not JSON: refused.
	if again, err := SetCgroupParent(got, ContainerSlice); err != nil || string(again) != string(got) {
		t.Fatalf("idempotent: %s %v", again, err)
	}
	if _, err := SetCgroupParent([]byte(`{"cgroup-parent": "mine.slice"}`), ContainerSlice); err == nil || !strings.Contains(err.Error(), "mine.slice") {
		t.Fatalf("%v", err)
	}
	if _, err := SetCgroupParent([]byte(`{nope`), ContainerSlice); err == nil {
		t.Fatal("not JSON")
	}
}

func TestUnsetCgroupParent(t *testing.T) {
	out, empty, err := UnsetCgroupParent([]byte(`{"cgroup-parent": "agent-containers.slice"}`), "")
	if err != nil || !empty || string(out) != "{\n}\n" {
		t.Fatalf("%q %v %v", out, empty, err)
	}
	out, empty, _ = UnsetCgroupParent([]byte(`{"cgroup-parent": "agent-containers.slice", "debug": true}`), "old.slice")
	var m map[string]any
	if json.Unmarshal(out, &m); empty || m["cgroup-parent"] != "old.slice" || m["debug"] != true {
		t.Fatalf("%s", out)
	}
}

// rootCmd carries out sudo install and sudo rm inside the test's root, and
// records every call (fakeCmd answers the rest).
type rootCmd struct {
	*fakeCmd
	t *testing.T
}

func (r rootCmd) Run(env []string, name string, args ...string) (string, error) {
	out, err := r.fakeCmd.Run(env, name, args...)
	if name != "sudo" || len(args) == 0 {
		return out, err
	}
	switch args[0] {
	case "install": // install -D -m MODE SRC DST
		src, dst := args[len(args)-2], args[len(args)-1]
		b, rerr := os.ReadFile(src)
		if rerr != nil {
			r.t.Fatal(rerr)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			r.t.Fatal(err)
		}
	case "rm": // rm -f PATH
		_ = os.Remove(args[len(args)-1])
	}
	return out, err
}

func hostSetup(t *testing.T) (*Installer, *fakeCmd) {
	in, cmd, _, _ := setup(t)
	in.Root = t.TempDir()
	in.User = "m"
	in.Cmd = rootCmd{cmd, t}
	cmd.outputs["docker info --format {{.CgroupDriver}}"] = "systemd\n"
	cmd.outputs["loginctl show-user m -p Linger --value"] = "no\n"
	return in, cmd
}

func TestDockerStepCapsContainersAndUndoes(t *testing.T) {
	in, cmd := hostSetup(t)
	daemon := in.daemonJSON()
	if err := os.MkdirAll(filepath.Dir(daemon), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(daemon, []byte(`{"log-driver": "journald"}`), 0o644)
	if err := in.Install(map[string]bool{StepDocker: true}); err != nil {
		t.Fatal(err)
	}
	slice, err := os.ReadFile(filepath.Join(in.sliceDir(), ContainerSlice))
	if err != nil || !strings.Contains(string(slice), "MemoryMax=25%") {
		t.Fatalf("slice unit: %v\n%s", err, slice)
	}
	var m map[string]any
	b, _ := os.ReadFile(daemon)
	if json.Unmarshal(b, &m); m["cgroup-parent"] != ContainerSlice || m["log-driver"] != "journald" {
		t.Fatalf("daemon.json: %s", b)
	}
	calls := strings.Join(cmd.calls, "\n")
	if !strings.Contains(calls, "sudo systemctl daemon-reload\nsudo systemctl restart docker") {
		t.Fatalf("calls:\n%s", calls)
	}
	// Uninstall puts daemon.json back as it was (other settings kept) and
	// removes the slice.
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(daemon)
	m = nil
	if json.Unmarshal(b, &m); m["cgroup-parent"] != nil || m["log-driver"] != "journald" {
		t.Fatalf("restored daemon.json: %s", b)
	}
	if _, err := os.Stat(filepath.Join(in.sliceDir(), ContainerSlice)); !os.IsNotExist(err) {
		t.Fatal("slice unit left")
	}
}

func TestDockerStepRemovesADaemonJSONItCreated(t *testing.T) {
	in, _ := hostSetup(t)
	if err := in.Install(map[string]bool{StepDocker: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(in.daemonJSON()); err != nil {
		t.Fatal(err)
	}
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(in.daemonJSON()); !os.IsNotExist(err) {
		t.Fatal("daemon.json cccp created was left behind")
	}
}

func TestDockerStepRefusesWithoutTheSystemdDriver(t *testing.T) {
	in, cmd := hostSetup(t)
	cmd.outputs["docker info --format {{.CgroupDriver}}"] = "cgroupfs\n"
	if err := in.Install(map[string]bool{StepDocker: true}); err == nil || !strings.Contains(err.Error(), "cgroupfs") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(in.daemonJSON()); !os.IsNotExist(err) {
		t.Fatal("wrote daemon.json anyway")
	}
}

// Host steps never run unless named, even with --yes.
func TestHostStepsOnlyWhenNamed(t *testing.T) {
	in, cmd := hostSetup(t)
	if err := in.Install(nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range cmd.calls {
		if strings.HasPrefix(c, "sudo ") || strings.HasPrefix(c, "loginctl enable") || strings.HasPrefix(c, "docker ") {
			t.Fatalf("a plain install ran %q", c)
		}
	}
}

func TestLingerStepRestoresWhatWasThere(t *testing.T) {
	in, cmd := hostSetup(t)
	if err := in.Install(map[string]bool{StepLinger: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cmd.calls, "\n"), "loginctl enable-linger") {
		t.Fatalf("%v", cmd.calls)
	}
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cmd.calls, "\n"), "loginctl disable-linger") {
		t.Fatal("linger was off before: uninstall turns it off")
	}
	// Linger already on before cccp: uninstall leaves it on.
	in2, cmd2 := hostSetup(t)
	cmd2.outputs["loginctl show-user m -p Linger --value"] = "yes\n"
	if err := in2.Install(map[string]bool{StepLinger: true}); err != nil {
		t.Fatal(err)
	}
	if err := in2.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(cmd2.calls, "\n"), "disable-linger") {
		t.Fatal("turned off a linger the user had")
	}
}

func TestContainerCheck(t *testing.T) {
	ours := []byte(`{"cgroup-parent": "agent-containers.slice"}`)
	for _, c := range []struct {
		daemon []byte
		max    string
		ok     bool
		says   string
	}{
		{ours, "23003000000\n", true, "MemoryMax 23003000000"},
		{ours, "infinity", false, "no memory limit"},
		{ours, "", false, "no memory limit"},
		{nil, "23003000000", false, "outside the jail"},
		{[]byte(`{"cgroup-parent": "other.slice"}`), "1", false, "outside the jail"},
	} {
		ok, detail := ContainerCheck(c.daemon, c.max)
		if ok != c.ok || !strings.Contains(detail, c.says) {
			t.Errorf("%s / %q: %v %q", c.daemon, c.max, ok, detail)
		}
	}
}

// Installing twice keeps what was there before the first install, so
// uninstall still restores the user's own state.
func TestDockerStepTwiceStillRestoresTheOriginal(t *testing.T) {
	in, _ := hostSetup(t)
	os.MkdirAll(filepath.Dir(in.daemonJSON()), 0o755)
	os.WriteFile(in.daemonJSON(), []byte(`{"debug": true}`), 0o644)
	for i := 0; i < 2; i++ {
		if err := in.Install(map[string]bool{StepDocker: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	b, _ := os.ReadFile(in.daemonJSON())
	if json.Unmarshal(b, &m); m["cgroup-parent"] != nil || m["debug"] != true {
		t.Fatalf("%s", b)
	}
	in2, cmd2 := hostSetup(t)
	for i := 0; i < 2; i++ {
		if err := in2.Install(map[string]bool{StepLinger: true}); err != nil {
			t.Fatal(err)
		}
		cmd2.outputs["loginctl show-user m -p Linger --value"] = "yes\n" // on after the first
	}
	if err := in2.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cmd2.calls, "\n"), "disable-linger") {
		t.Fatal("linger was off before the first install: uninstall turns it off")
	}
}

// A daemon.json the user had stays, even when it is empty again.
func TestDockerStepKeepsAnEmptyDaemonJSONTheUserHad(t *testing.T) {
	in, _ := hostSetup(t)
	os.MkdirAll(filepath.Dir(in.daemonJSON()), 0o755)
	os.WriteFile(in.daemonJSON(), []byte("{}\n"), 0o644)
	if err := in.Install(map[string]bool{StepDocker: true}); err != nil {
		t.Fatal(err)
	}
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(in.daemonJSON()); err != nil {
		t.Fatalf("removed the user's daemon.json: %v", err)
	}
}
