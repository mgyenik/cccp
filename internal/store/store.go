// Package store reads and writes hook-observed session state files.
//
// Writers (`cccp hook`) hold an exclusive flock on a single lock file for the
// read-modify-write cycle and replace the state file by atomic rename, so
// readers never see a partial file and never need the lock.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"cccp/internal/model"
	"cccp/internal/paths"
)

// FileName returns the state file name for a session.
func FileName(agent model.Agent, sessionID string) string {
	return string(agent) + "-" + sanitize(sessionID) + ".json"
}

func sanitize(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
}

// Path returns the absolute state file path for a session.
func Path(agent model.Agent, sessionID string) string {
	return filepath.Join(paths.SessionsDir(), FileName(agent, sessionID))
}

// Update applies fn to the session's state under the lock and writes the
// result atomically. The lock wait is bounded so a wedged writer cannot stall
// the agent that runs the hook.
func Update(agent model.Agent, sessionID string, fn func(model.HookState) model.HookState) error {
	dir := paths.SessionsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(paths.SessionsLock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := lockWithTimeout(int(lf.Fd()), 2*time.Second); err != nil {
		return err
	}
	defer unix.Flock(int(lf.Fd()), unix.LOCK_UN)

	p := Path(agent, sessionID)
	var cur model.HookState
	if b, err := os.ReadFile(p); err == nil {
		// A corrupt file is replaced rather than wedging the session.
		_ = json.Unmarshal(b, &cur)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	next := fn(cur)
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return WriteAtomic(p, b)
}

func lockWithTimeout(fd int, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", paths.SessionsLock())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// WriteAtomic writes b to p via a temp file in the same directory and rename.
// Temp files start with '.' so directory watchers can ignore them.
func WriteAtomic(p string, b []byte) error {
	dir := filepath.Dir(p)
	f, err := os.CreateTemp(dir, "."+filepath.Base(p)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Load reads one state file.
func Load(p string) (model.HookState, error) {
	var s model.HookState
	b, err := os.ReadFile(p)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", p, err)
	}
	return s, nil
}

// IsStateFile reports whether a directory entry name is a session state file.
func IsStateFile(name string) bool {
	return strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".")
}

// LoadAll reads every state file. Unreadable files are skipped and reported.
func LoadAll() (map[string]model.HookState, []error) {
	out := map[string]model.HookState{}
	var errs []error
	ents, err := os.ReadDir(paths.SessionsDir())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		return out, errs
	}
	for _, e := range ents {
		if e.IsDir() || !IsStateFile(e.Name()) {
			continue
		}
		p := filepath.Join(paths.SessionsDir(), e.Name())
		s, err := Load(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out[p] = s
	}
	return out, errs
}

// Remove deletes a session's state file.
func Remove(p string) error {
	err := os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
