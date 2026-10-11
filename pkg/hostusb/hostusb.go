// Package hostusb lists the host's USB devices a VM can be given, from
// /sys/bus/usb/devices, and says which ones the host needs itself: a network
// card, a disk that is mounted. Hubs are not devices to give away.
package hostusb

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SysBusUSB is where the kernel lists USB devices and their interfaces.
const SysBusUSB = "/sys/bus/usb/devices"

// Device is a USB device plugged into the host.
type Device struct {
	Vendor  string // 4 hex digits
	Product string // 4 hex digits
	Name    string
	Bus     int
	Address int
	// InUse says what the host uses the device for; empty when it is free to
	// give to a VM
	InUse string
}

// ID is the device's vendor:product, which a VM is given the device by.
func (d Device) ID() string { return d.Vendor + ":" + d.Product }

// List reads the devices under sys (SysBusUSB, or a copy in tests). mounts is
// the content of /proc/mounts, which says whether a disk is in use.
func List(sys string, mounts string) ([]Device, error) {
	entries, err := os.ReadDir(sys)
	if err != nil {
		return nil, err
	}

	mounted := mountedSources(mounts)

	var out []Device
	for _, e := range entries {
		name := e.Name()
		// interfaces (1-2:1.0) and root hubs (usb1) are not devices to give
		if strings.Contains(name, ":") || strings.HasPrefix(name, "usb") {
			continue
		}

		dir := filepath.Join(sys, name)
		if read(dir, "bDeviceClass") == "09" {
			continue // a hub
		}

		d := Device{
			Vendor:  read(dir, "idVendor"),
			Product: read(dir, "idProduct"),
			Bus:     atoi(read(dir, "busnum")),
			Address: atoi(read(dir, "devnum")),
		}
		if d.Vendor == "" || d.Product == "" {
			continue
		}

		d.Name = deviceName(read(dir, "manufacturer"), read(dir, "product"))
		if d.Name == "" {
			d.Name = fmt.Sprintf("USB device %s", d.ID())
		}

		d.InUse = inUse(dir, mounted)
		out = append(out, d)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Bus != out[j].Bus {
			return out[i].Bus < out[j].Bus
		}
		return out[i].Address < out[j].Address
	})

	return out, nil
}

// inUse walks a device's directory, not following links, for what the host
// made of it: a network interface, or a disk with something mounted.
func inUse(dir string, mounted map[string]bool) string {
	var reason string

	var walk func(path string, depth int)
	walk = func(path string, depth int) {
		if reason != "" || depth > 8 {
			return
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			return
		}

		for _, e := range entries {
			if !e.IsDir() || e.Type()&os.ModeSymlink != 0 {
				continue
			}

			sub := filepath.Join(path, e.Name())
			switch e.Name() {
			case "net":
				if names := dirNames(sub); len(names) > 0 {
					reason = "network card " + strings.Join(names, ", ")
					return
				}
			case "block":
				for _, disk := range dirNames(sub) {
					if mounted[disk] {
						reason = "disk " + disk + ", mounted"
						return
					}
				}
			}

			walk(sub, depth+1)
		}
	}

	walk(dir, 0)
	return reason
}

// mountedSources are the block devices with something mounted, by their
// kernel name and their disk's: /dev/sda1 mounted counts for sda1 and sda.
func mountedSources(mounts string) map[string]bool {
	out := map[string]bool{}

	sc := bufio.NewScanner(strings.NewReader(mounts))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}

		dev := strings.TrimPrefix(fields[0], "/dev/")
		out[dev] = true
		out[strings.TrimRight(dev, "0123456789")] = true
	}

	return out
}

// deviceName joins a device's manufacturer and product strings, once: many devices
// repeat the one in the other ("USB Storage USB Storage").
func deviceName(manufacturer, product string) string {
	switch {
	case product == "":
		return manufacturer
	case manufacturer == "" || strings.HasPrefix(strings.ToLower(product), strings.ToLower(manufacturer)):
		return product
	}
	return manufacturer + " " + product
}

func dirNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func read(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
