// Package power reports whether the machine runs on battery. It is read on
// demand (when events arrive), never polled.
package power

import (
	"os"
	"path/filepath"
	"strings"
)

const root = "/sys/class/power_supply"

// OnBattery is true when the machine has a mains supply and it is offline.
// Machines without a mains supply entry (desktops) report false.
func OnBattery() bool {
	ents, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	sawMains := false
	for _, e := range ents {
		dir := filepath.Join(root, e.Name())
		if read(filepath.Join(dir, "type")) != "Mains" {
			continue
		}
		sawMains = true
		if read(filepath.Join(dir, "online")) == "1" {
			return false
		}
	}
	return sawMains
}

func read(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
