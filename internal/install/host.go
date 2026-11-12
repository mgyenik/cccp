package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Host steps change system configuration, so they run only when named
// (cccp install --only docker,linger), and what they replace is recorded in
// L.HostRestore for uninstall.
const (
	// StepDocker caps the memory of containers started through Docker.
	StepDocker = "docker"
	// StepLinger keeps the user manager (tmux, cccp, agents) running
	// without a login session.
	StepLinger = "linger"
)

// HostSteps are opt-in: never part of a plain install.
var HostSteps = map[string]bool{StepDocker: true, StepLinger: true}

// ContainerSlice is the system slice containers are started under.
const ContainerSlice = "agent-containers.slice"

// System paths the docker step changes.
const (
	DockerDaemonJSON  = "/etc/docker/daemon.json"
	ContainerSliceDir = "/etc/systemd/system"
)

// daemonJSON and sliceDir are the system paths, under Root (tests).
func (i *Installer) daemonJSON() string { return filepath.Join(i.Root, DockerDaemonJSON) }
func (i *Installer) sliceDir() string   { return filepath.Join(i.Root, ContainerSliceDir) }

// hostRestore is what the host steps changed, for uninstall.
type hostRestore struct {
	Docker *dockerRestore `json:"docker,omitempty"`
	// LingerBefore is "yes"/"no" before the linger step ran ("" if it did
	// not run).
	LingerBefore string `json:"linger_before,omitempty"`
}

type dockerRestore struct {
	// DaemonJSONExisted: /etc/docker/daemon.json existed before.
	DaemonJSONExisted bool `json:"daemon_json_existed"`
	// PrevParent is its cgroup-parent before ("" if none).
	PrevParent string `json:"prev_parent,omitempty"`
}

// SetCgroupParent sets "cgroup-parent" in a daemon.json (empty: none yet),
// keeping every other setting. It refuses to replace a different parent the
// file already sets: someone chose it.
func SetCgroupParent(daemonJSON []byte, slice string) ([]byte, error) {
	m, err := parseObject(daemonJSON)
	if err != nil {
		return nil, err
	}
	if raw, ok := m["cgroup-parent"]; ok {
		var cur string
		if json.Unmarshal(raw, &cur) != nil || cur != slice {
			return nil, fmt.Errorf("%s already sets cgroup-parent to %s; not changing it", DockerDaemonJSON, string(raw))
		}
	}
	v, _ := json.Marshal(slice)
	m["cgroup-parent"] = v
	return encodeObject(m), nil
}

// UnsetCgroupParent puts "cgroup-parent" back to prev ("" removes it), and
// reports whether the object is then empty.
func UnsetCgroupParent(daemonJSON []byte, prev string) (out []byte, empty bool, err error) {
	m, err := parseObject(daemonJSON)
	if err != nil {
		return nil, false, err
	}
	if prev == "" {
		delete(m, "cgroup-parent")
	} else {
		v, _ := json.Marshal(prev)
		m["cgroup-parent"] = v
	}
	return encodeObject(m), len(m) == 0, nil
}

func parseObject(b []byte) (map[string]json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(b)) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %v", DockerDaemonJSON, err)
	}
	return m, nil
}

// encodeObject writes keys sorted, values as they were.
func encodeObject(m map[string]json.RawMessage) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, k := range keys {
		kb, _ := json.Marshal(k)
		var v bytes.Buffer
		if json.Indent(&v, m[k], "  ", "  ") != nil {
			v.Reset()
			v.Write(m[k])
		}
		fmt.Fprintf(&b, "  %s: %s", kb, v.Bytes())
		if i < len(keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// ---- docker -----------------------------------------------------------------

// dockerWhat describes the docker step, naming running containers (a
// restart of dockerd stops them).
func (i *Installer) dockerWhat() string {
	what := "cap the memory of every container started through Docker (25% of RAM, docs/jails.md):" +
		"\n    adds " + filepath.Join(ContainerSliceDir, ContainerSlice) + " and \"cgroup-parent\": \"" + ContainerSlice + "\" to " + DockerDaemonJSON +
		"\n    then restarts dockerd (needs sudo)"
	if out, err := i.Cmd.Run(nil, "docker", "ps", "-q"); err == nil {
		if n := len(strings.Fields(out)); n > 0 {
			what += fmt.Sprintf("\n    the restart stops the %d running container(s)", n)
		}
	}
	return what
}

func (i *Installer) installDocker() error {
	out, err := i.Cmd.Run(nil, "docker", "info", "--format", "{{.CgroupDriver}}")
	if err != nil {
		return fmt.Errorf("docker info: %v: %s", err, strings.TrimSpace(out))
	}
	if d := strings.TrimSpace(out); d != "systemd" {
		return fmt.Errorf("dockerd uses the %q cgroup driver; a slice as cgroup-parent needs \"systemd\"", d)
	}
	cur, err := os.ReadFile(i.daemonJSON())
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	prev := ""
	if m, err := parseObject(cur); err == nil {
		_ = json.Unmarshal(m["cgroup-parent"], &prev)
	}
	next, err := SetCgroupParent(cur, ContainerSlice)
	if err != nil {
		return err
	}
	unit, err := fs.ReadFile(i.Assets, "integrations/docker/agent-containers.slice")
	if err != nil {
		return err
	}
	if err := i.saveHostRestore(func(r *hostRestore) {
		if r.Docker == nil { // the first install's state is the one to restore
			r.Docker = &dockerRestore{DaemonJSONExisted: existed, PrevParent: prev}
		}
	}); err != nil {
		return err
	}
	if err := i.sudoWrite(filepath.Join(i.sliceDir(), ContainerSlice), unit); err != nil {
		return err
	}
	if err := i.sudoWrite(i.daemonJSON(), next); err != nil {
		return err
	}
	for _, args := range [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "restart", "docker"}} {
		if out, err := i.Cmd.Run(nil, "sudo", args...); err != nil {
			return fmt.Errorf("sudo %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

func (i *Installer) uninstallDocker() error {
	r, err := i.loadHostRestore()
	if err != nil {
		return err
	}
	if r.Docker == nil {
		i.printf("  not installed by cccp; nothing to undo\n")
		return nil
	}
	cur, err := os.ReadFile(i.daemonJSON())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		next, empty, err := UnsetCgroupParent(cur, r.Docker.PrevParent)
		if err != nil {
			return err
		}
		if empty && !r.Docker.DaemonJSONExisted {
			if out, err := i.Cmd.Run(nil, "sudo", "rm", "-f", i.daemonJSON()); err != nil {
				return fmt.Errorf("sudo rm: %v: %s", err, out)
			}
		} else if err := i.sudoWrite(i.daemonJSON(), next); err != nil {
			return err
		}
	}
	if out, err := i.Cmd.Run(nil, "sudo", "rm", "-f", filepath.Join(i.sliceDir(), ContainerSlice)); err != nil {
		return fmt.Errorf("sudo rm: %v: %s", err, out)
	}
	for _, args := range [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "restart", "docker"}} {
		if out, err := i.Cmd.Run(nil, "sudo", args...); err != nil {
			return fmt.Errorf("sudo %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	return i.saveHostRestore(func(r *hostRestore) { r.Docker = nil })
}

// ---- linger -----------------------------------------------------------------

func (i *Installer) installLinger() error {
	before := i.lingerState()
	if err := i.saveHostRestore(func(r *hostRestore) {
		if r.LingerBefore == "" {
			r.LingerBefore = before
		}
	}); err != nil {
		return err
	}
	if out, err := i.Cmd.Run(nil, "loginctl", "enable-linger"); err != nil {
		return fmt.Errorf("loginctl enable-linger: %v: %s", err, out)
	}
	return nil
}

func (i *Installer) uninstallLinger() error {
	r, err := i.loadHostRestore()
	if err != nil {
		return err
	}
	switch r.LingerBefore {
	case "":
		i.printf("  not enabled by cccp; nothing to undo\n")
		return nil
	case "no":
		if out, err := i.Cmd.Run(nil, "loginctl", "disable-linger"); err != nil {
			return fmt.Errorf("loginctl disable-linger: %v: %s", err, out)
		}
	}
	return i.saveHostRestore(func(r *hostRestore) { r.LingerBefore = "" })
}

// lingerState is "yes" or "no" ("" if unknown).
func (i *Installer) lingerState() string {
	out, err := i.Cmd.Run(nil, "loginctl", "show-user", i.User, "-p", "Linger", "--value")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ---- files ------------------------------------------------------------------

// sudoWrite writes data to a root-owned path: through a file of the user's,
// then sudo install (sudo asks for a password on the terminal).
func (i *Installer) sudoWrite(dst string, data []byte) error {
	tmp, err := os.CreateTemp("", "cccp-host-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if out, err := i.Cmd.Run(nil, "sudo", "install", "-D", "-m", "0644", tmp.Name(), dst); err != nil {
		return fmt.Errorf("sudo install %s: %v: %s", dst, err, out)
	}
	return nil
}

func (i *Installer) loadHostRestore() (hostRestore, error) {
	var r hostRestore
	b, err := os.ReadFile(i.L.HostRestore)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

func (i *Installer) saveHostRestore(change func(*hostRestore)) error {
	r, err := i.loadHostRestore()
	if err != nil {
		return err
	}
	change(&r)
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := os.MkdirAll(filepath.Dir(i.L.HostRestore), 0o755); err != nil {
		return err
	}
	return os.WriteFile(i.L.HostRestore, append(b, '\n'), 0o644)
}

// ContainerCheck is cccp doctor's verdict on containers: capped when
// daemon.json makes ContainerSlice their default parent and the slice has a
// memory limit (memoryMax is the slice's MemoryMax as systemctl shows it,
// "infinity" or bytes).
func ContainerCheck(daemonJSON []byte, memoryMax string) (ok bool, detail string) {
	parent := ""
	if m, err := parseObject(daemonJSON); err == nil {
		_ = json.Unmarshal(m["cgroup-parent"], &parent)
	}
	memoryMax = strings.TrimSpace(memoryMax)
	switch {
	case parent != ContainerSlice:
		return false, "containers run outside the jail with no memory limit (Docker's own cgroups): cccp install --only docker"
	case memoryMax == "" || memoryMax == "infinity":
		return false, ContainerSlice + " has no memory limit (its unit is missing or not loaded): cccp install --only docker"
	}
	return true, "containers default into " + ContainerSlice + ", MemoryMax " + memoryMax + " bytes"
}
