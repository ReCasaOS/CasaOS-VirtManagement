// Package hostnet lists the host's network devices a VM can be put on: its
// own Linux bridges, and the wired cards macvtap can sit on. It only reads
// /sys/class/net; CasaOS never creates or changes the host's network.
package hostnet

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SysClassNet is where the kernel lists network devices.
const SysClassNet = "/sys/class/net"

// Devices is what a VM can be put on.
type Devices struct {
	Bridges []string // Linux bridges the host's own network is on
	Links   []string // wired cards, not in a bridge, for macvtap
}

// Read lists the devices under sys (SysClassNet, or a copy in tests). The
// bridges that Docker, libvirt and CasaOS make for their own networks are left
// out, as are the names in skip.
func Read(sys string, skip ...string) (Devices, error) {
	entries, err := os.ReadDir(sys)
	if err != nil {
		return Devices{}, err
	}

	var d Devices
	for _, e := range entries {
		name := e.Name()
		dir := filepath.Join(sys, name)

		switch {
		case slices.Contains(skip, name) || internal(name):
		case exists(filepath.Join(dir, "bridge")):
			d.Bridges = append(d.Bridges, name)
		case wired(dir):
			d.Links = append(d.Links, name)
		}
	}

	return d, nil
}

// internal reports whether name is a device some other software makes for
// its own networks.
func internal(name string) bool {
	return name == "lo" || name == "docker0" || name == "docker_gwbridge" ||
		strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "virbr") ||
		strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "vnet") ||
		strings.HasPrefix(name, "macvtap")
}

// wired reports whether dir is a physical card, not Wi-Fi, and not a port of
// a bridge or a bond: macvtap on a Wi-Fi card gets no traffic from the access
// point, and on a bridge port it bypasses the bridge.
func wired(dir string) bool {
	return exists(filepath.Join(dir, "device")) &&
		!exists(filepath.Join(dir, "wireless")) && !exists(filepath.Join(dir, "phy80211")) &&
		!exists(filepath.Join(dir, "master"))
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
