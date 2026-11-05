// Package systemd configures cccp's slices and moves processes into them
// through the user's systemd manager, over D-Bus (docs/jails.md).
package systemd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/godbus/dbus/v5"

	"cccp/internal/jail"
)

// Manager is the user's systemd manager.
type Manager struct {
	obj dbus.BusObject
}

var errInTest = errors.New("refusing to reconfigure the real systemd from a test binary")

// Connect connects to the user's systemd manager on the session bus.
func Connect() (*Manager, error) {
	if testing.Testing() {
		return nil, errInTest
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}
	return &Manager{obj: conn.Object("org.freedesktop.systemd1", "/org/freedesktop/systemd1")}, nil
}

const iface = "org.freedesktop.systemd1.Manager."

// property is a unit property as D-Bus carries it: (sv).
type property struct {
	Name  string
	Value dbus.Variant
}

// auxUnit is StartTransientUnit's (unused) auxiliary units: a(sa(sv)).
type auxUnit struct {
	Name  string
	Props []property
}

func encode(ps []jail.Property) []property {
	out := make([]property, 0, len(ps))
	for _, p := range ps {
		out = append(out, property{p.Name, dbus.MakeVariant(p.Value)})
	}
	return out
}

func unitExists(err error) bool {
	var e dbus.Error
	return errors.As(err, &e) && e.Name == "org.freedesktop.systemd1.UnitExists"
}

// SetLimits gives a slice its limits: created with them if it does not
// exist, else changed in place (for this boot only: cccp, not a drop-in
// file, is where they are kept).
func (m *Manager) SetLimits(slice string, l jail.Limits) error {
	ps, err := jail.Properties(l)
	if err != nil {
		return err
	}
	props := encode(ps)
	err = m.obj.Call(iface+"StartTransientUnit", 0, slice, "fail", props, []auxUnit{}).Err
	if unitExists(err) {
		err = m.obj.Call(iface+"SetUnitProperties", 0, slice, true, props).Err
	}
	if err != nil {
		return fmt.Errorf("limits of %s: %w", slice, err)
	}
	return nil
}

// Move puts processes into a new scope in a slice; if the scope exists
// already (cccp's tmux scope, after the server restarted), into it.
func (m *Manager) Move(scope, slice string, pids []int) error {
	ps := append([]property{{"Slice", dbus.MakeVariant(slice)}, {"PIDs", dbus.MakeVariant(uint32s(pids))}}, encode(jail.ScopeProperties())...)
	err := m.obj.Call(iface+"StartTransientUnit", 0, scope, "fail", ps, []auxUnit{}).Err
	if unitExists(err) {
		return m.Attach(scope, pids)
	}
	if err != nil {
		return fmt.Errorf("moving %d processes to %s: %w", len(pids), scope, err)
	}
	return nil
}

// Attach adds processes to a running unit.
func (m *Manager) Attach(unit string, pids []int) error {
	if err := m.obj.Call(iface+"AttachProcessesToUnit", 0, unit, "", uint32s(pids)).Err; err != nil {
		return fmt.Errorf("attaching %d processes to %s: %w", len(pids), unit, err)
	}
	return nil
}

func uint32s(pids []int) []uint32 {
	out := make([]uint32, 0, len(pids))
	for _, p := range pids {
		if p > 0 {
			out = append(out, uint32(p))
		}
	}
	return out
}
