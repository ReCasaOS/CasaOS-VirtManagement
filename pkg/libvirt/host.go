package libvirt

import (
	"encoding/xml"
	"fmt"
	"slices"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// Host is what libvirt says about the machine it runs on.
type Host struct {
	LibvirtVersion string
	CPUs           int
	MemoryMiB      int
	KVM            bool
	UEFI           bool
}

// GetHost asks libvirt for its version, the host's CPUs and memory, and whether
// it can run KVM guests with UEFI firmware.
func GetHost(l *golibvirt.Libvirt) (Host, error) {
	var host Host

	version, err := l.ConnectGetLibVersion()
	if err != nil {
		return host, fmt.Errorf("get libvirt version: %w", err)
	}
	host.LibvirtVersion = FormatVersion(version)

	_, memoryKiB, cpus, _, _, _, _, _, err := l.NodeGetInfo()
	if err != nil {
		return host, fmt.Errorf("get node info: %w", err)
	}
	host.CPUs = int(cpus)
	host.MemoryMiB = int(memoryKiB / 1024)

	// Domain capabilities for KVM on the host's own architecture. libvirt
	// answers with an error when KVM is not usable (no /dev/kvm, no emulator).
	caps, err := l.ConnectGetDomainCapabilities(nil, nil, nil, golibvirt.OptString{"kvm"}, 0)
	if err != nil {
		return host, nil
	}
	host.KVM = true

	host.UEFI, err = SupportsUEFI(caps)
	if err != nil {
		return host, fmt.Errorf("parse domain capabilities: %w", err)
	}

	return host, nil
}

// FormatVersion turns libvirt's packed version (major*1000000 + minor*1000 +
// release) into "major.minor.release".
func FormatVersion(v uint64) string {
	return fmt.Sprintf("%d.%d.%d", v/1000000, v/1000%1000, v%1000)
}

type domainCapabilities struct {
	OS struct {
		Supported string `xml:"supported,attr"`
		Enums     []struct {
			Name   string   `xml:"name,attr"`
			Values []string `xml:"value"`
		} `xml:"enum"`
	} `xml:"os"`
}

// SupportsUEFI reports whether domain capabilities list "efi" among the
// firmware libvirt can pick by itself (<os firmware='efi'>), which it does from
// the QEMU firmware descriptors when OVMF or AAVMF is installed.
func SupportsUEFI(domainCapabilitiesXML string) (bool, error) {
	var caps domainCapabilities
	if err := xml.Unmarshal([]byte(domainCapabilitiesXML), &caps); err != nil {
		return false, err
	}

	if caps.OS.Supported != "yes" {
		return false, nil
	}

	for _, enum := range caps.OS.Enums {
		if enum.Name == "firmware" {
			return slices.Contains(enum.Values, "efi"), nil
		}
	}

	return false, nil
}
