package libvirt

import (
	"encoding/xml"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// MetadataNamespace names the element CasaOS keeps in a domain's <metadata>.
// A domain without it was not created by CasaOS: it is listed and can be
// started and stopped, but its disks are never deleted.
const MetadataNamespace = "https://recasaos.github.io/xmlns/vm/1"

// metadataPrefix is the prefix libvirt writes the element back with.
const metadataPrefix = "casaos"

const (
	FirmwareBIOS = "bios"
	FirmwareUEFI = "uefi"
)

// Metadata is what CasaOS remembers about a VM it created.
type Metadata struct {
	XMLName xml.Name `xml:"https://recasaos.github.io/xmlns/vm/1 vm"`

	// Volumes are the disk files CasaOS created for the VM, which delete may
	// remove.
	Volumes []string `xml:"volume"`

	Firmware string `xml:"firmware"`

	// Ports are forwarded from the host to the VM, on the CasaOS network.
	Ports []Port `xml:"port"`

	// OS is what the VM runs, for its icon: ubuntu, windows, …; empty when
	// nobody said.
	OS string `xml:"os"`

	// Snapshots of the VM's disks and UEFI variables, oldest first.
	Snapshots []Snapshot `xml:"snapshot"`
}

// Snapshot is a saved state of a VM that is off: a qcow2 internal snapshot
// named ID in every disk, and a copy of the UEFI variables.
type Snapshot struct {
	ID      string `xml:"id,attr"`
	Created string `xml:"created,attr"` // RFC 3339
	Name    string `xml:",chardata"`
}

// Port is a port forward: the host's port reaches the VM's.
type Port struct {
	Protocol string `xml:"protocol,attr"` // tcp or udp
	Host     int    `xml:"host,attr"`
	Guest    int    `xml:"guest,attr"`
}

// DomainSpec is a VM as CasaOS creates it.
type DomainSpec struct {
	Name      string // 8 hex digits, also the disk's file name
	Title     string
	VCPUs     int
	MemoryMiB int
	Firmware  string // FirmwareBIOS or FirmwareUEFI
	DiskPath  string // qcow2
	ISOPath   string // empty: the CD-ROM drive is there, but empty
	NIC       NIC
	Arch      string // x86_64 or aarch64
	OS        string // for the icon; empty: none
}

// NIC types: what libvirt calls the interface.
const (
	NICNetwork = "network" // on a libvirt network
	NICBridge  = "bridge"  // on a bridge of the host
	NICDirect  = "direct"  // macvtap on a network card of the host
)

// NIC is where a VM's network card goes.
type NIC struct {
	Type   string // NICNetwork, NICBridge or NICDirect
	Source string // the libvirt network, the host bridge, or the host card
	MAC    string // empty: libvirt picks one
}

// The domain XML is written from these structs, so no user input is ever
// concatenated into XML.

type domainXML struct {
	XMLName    xml.Name     `xml:"domain"`
	Type       string       `xml:"type,attr"`
	Name       string       `xml:"name"`
	Title      string       `xml:"title,omitempty"`
	Metadata   *metadataXML `xml:"metadata"`
	Memory     memoryXML    `xml:"memory"`
	VCPU       int          `xml:"vcpu"`
	OS         osXML        `xml:"os"`
	Features   *featuresXML `xml:"features"`
	CPU        cpuXML       `xml:"cpu"`
	Clock      clockXML     `xml:"clock"`
	OnPoweroff string       `xml:"on_poweroff"`
	OnReboot   string       `xml:"on_reboot"`
	OnCrash    string       `xml:"on_crash"`
	Devices    devicesXML   `xml:"devices"`
}

type metadataXML struct {
	VM *metadataOutXML
}

// metadataOutXML is Metadata written with a namespace prefix, which libvirt
// requires of <metadata> elements.
type metadataOutXML struct {
	XMLName  xml.Name `xml:"casaos:vm"`
	XMLNS    string   `xml:"xmlns:casaos,attr"`
	Volumes  []string `xml:"casaos:volume"`
	Firmware string   `xml:"casaos:firmware"`
	Ports    []Port   `xml:"casaos:port"`
	OS       string   `xml:"casaos:os,omitempty"`
}

type memoryXML struct {
	Unit  string `xml:"unit,attr"`
	Value int    `xml:",chardata"`
}

type osXML struct {
	Firmware   string       `xml:"firmware,attr,omitempty"`
	Type       osTypeXML    `xml:"type"`
	FirmwareEl *firmwareXML `xml:"firmware"`
	Loader     *loaderXML   `xml:"loader"`
}

type osTypeXML struct {
	Arch    string `xml:"arch,attr"`
	Machine string `xml:"machine,attr"`
	Value   string `xml:",chardata"`
}

type firmwareXML struct {
	Features []firmwareFeatureXML `xml:"feature"`
}

type firmwareFeatureXML struct {
	Enabled string `xml:"enabled,attr"`
	Name    string `xml:"name,attr"`
}

type loaderXML struct {
	Type string `xml:"type,attr,omitempty"`
}

type featuresXML struct {
	ACPI *struct{} `xml:"acpi"`
	APIC *struct{} `xml:"apic,omitempty"`
}

type cpuXML struct {
	Mode string `xml:"mode,attr"`
}

type clockXML struct {
	Offset string `xml:"offset,attr"`
}

type devicesXML struct {
	Controllers []controllerXML `xml:"controller"`
	Disks       []diskXML       `xml:"disk"`
	Interfaces  []interfaceXML  `xml:"interface"`
	Graphics    []graphicsXML   `xml:"graphics"`
	Video       *videoXML       `xml:"video"`
	Inputs      []inputXML      `xml:"input"`
	RNG         *rngXML         `xml:"rng"`
	Balloon     *balloonXML     `xml:"memballoon"`
}

// balloonXML is the memory balloon. Without a stats period QEMU never asks the
// guest driver for its memory figures.
type balloonXML struct {
	Model string          `xml:"model,attr"`
	Stats balloonStatsXML `xml:"stats"`
}

type balloonStatsXML struct {
	Period int `xml:"period,attr"`
}

type controllerXML struct {
	Type  string `xml:"type,attr"`
	Index string `xml:"index,attr,omitempty"`
	Model string `xml:"model,attr"`
}

type diskXML struct {
	XMLName  xml.Name       `xml:"disk"`
	Type     string         `xml:"type,attr"`
	Device   string         `xml:"device,attr"`
	Driver   *diskDriverXML `xml:"driver"`
	Source   *diskSourceXML `xml:"source"`
	Target   diskTargetXML  `xml:"target"`
	ReadOnly *struct{}      `xml:"readonly"`
	Boot     *bootXML       `xml:"boot"`
}

type diskDriverXML struct {
	Name    string `xml:"name,attr"`
	Type    string `xml:"type,attr"`
	Cache   string `xml:"cache,attr,omitempty"`
	Discard string `xml:"discard,attr,omitempty"`
}

type diskSourceXML struct {
	File     string       `xml:"file,attr,omitempty"`
	Seclabel *seclabelXML `xml:"seclabel"`
}

type seclabelXML struct {
	Model   string `xml:"model,attr"`
	Relabel string `xml:"relabel,attr"`
}

type diskTargetXML struct {
	Dev string `xml:"dev,attr"`
	Bus string `xml:"bus,attr"`
}

type bootXML struct {
	Order int `xml:"order,attr"`
}

type interfaceXML struct {
	XMLName xml.Name           `xml:"interface"`
	Type    string             `xml:"type,attr"`
	MAC     *macXML            `xml:"mac"`
	Source  interfaceSourceXML `xml:"source"`
	Model   *modelXML          `xml:"model"`
}

type macXML struct {
	Address string `xml:"address,attr"`
}

type interfaceSourceXML struct {
	Network string `xml:"network,attr,omitempty"`
	Bridge  string `xml:"bridge,attr,omitempty"`
	Dev     string `xml:"dev,attr,omitempty"`
	Mode    string `xml:"mode,attr,omitempty"`
}

type modelXML struct {
	Type string `xml:"type,attr"`
}

type graphicsXML struct {
	Type   string             `xml:"type,attr"`
	Socket string             `xml:"socket,attr,omitempty"`
	Listen *graphicsListenXML `xml:"listen"`
}

type graphicsListenXML struct {
	Type   string `xml:"type,attr"`
	Socket string `xml:"socket,attr,omitempty"`
}

type videoXML struct {
	Model modelXML `xml:"model"`
}

type inputXML struct {
	Type string `xml:"type,attr"`
	Bus  string `xml:"bus,attr"`
}

type rngXML struct {
	Model   string        `xml:"model,attr"`
	Backend rngBackendXML `xml:"backend"`
}

type rngBackendXML struct {
	Model string `xml:"model,attr"`
	Value string `xml:",chardata"`
}

// CDROMTarget is the CD-ROM drive's target, the handle to change its media.
const CDROMTarget = "sda"

// BootDiskTarget is the target of the disk a CasaOS VM boots from.
const BootDiskTarget = "vda"

// pcieRootPorts is how many PCIe root ports a new VM gets. libvirt adds one
// per PCI device it defines, and none to spare, so without these a disk could
// not be plugged into a running VM. The VM's own devices take five (USB
// controller, disk, NIC, balloon, RNG; measured with libvirt 12), leaving seven.
const pcieRootPorts = 12

// balloonStatsPeriod is how often, in seconds, QEMU refreshes the guest memory
// statistics; the sampler reads them every few seconds.
const balloonStatsPeriod = 5

// BuildDomainXML writes the libvirt definition of a new VM.
//
//   - KVM, q35 (virt on arm64), the host CPU passed through
//   - UEFI: libvirt picks the firmware from the QEMU descriptors, restricted to
//     flash images without secure boot. Without type='pflash', Ubuntu's
//     descriptors make it pick the AMD SEV build, which AppArmor refuses.
//   - one qcow2 disk on virtio with writeback cache (O_DIRECT fails on
//     mergerfs), and a SATA CD-ROM drive that is always there, so an ISO can
//     be inserted or ejected while the VM runs; the CD-ROM boots first when
//     it holds an ISO
//   - virtio NIC on the NAT network
//   - enough PCIe root ports to plug disks into the running VM
//   - VNC on a unix socket libvirt creates in the domain's private directory:
//     no TCP port, not even on loopback
func BuildDomainXML(spec DomainSpec) (string, error) {
	machine := "q35"
	if spec.Arch == "aarch64" {
		machine = "virt"
	}

	d := domainXML{
		Type:  "kvm",
		Name:  spec.Name,
		Title: spec.Title,
		Metadata: &metadataXML{VM: &metadataOutXML{
			XMLNS:    MetadataNamespace,
			Volumes:  []string{spec.DiskPath},
			Firmware: spec.Firmware,
			OS:       spec.OS,
		}},
		Memory:     memoryXML{Unit: "MiB", Value: spec.MemoryMiB},
		VCPU:       spec.VCPUs,
		OS:         osXML{Type: osTypeXML{Arch: spec.Arch, Machine: machine, Value: "hvm"}},
		Features:   &featuresXML{ACPI: &struct{}{}},
		CPU:        cpuXML{Mode: "host-passthrough"},
		Clock:      clockXML{Offset: "utc"},
		OnPoweroff: "destroy",
		OnReboot:   "restart",
		OnCrash:    "destroy",
	}

	if spec.Arch == "x86_64" {
		d.Features.APIC = &struct{}{}
	}

	switch spec.Firmware {
	case FirmwareUEFI:
		d.OS.Firmware = "efi"
		d.OS.FirmwareEl = &firmwareXML{Features: []firmwareFeatureXML{{Enabled: "no", Name: "secure-boot"}}}
		d.OS.Loader = &loaderXML{Type: "pflash"}
	case FirmwareBIOS:
		if spec.Arch != "x86_64" {
			return "", fmt.Errorf("firmware %q is not available on %s", spec.Firmware, spec.Arch)
		}
	default:
		return "", fmt.Errorf("unknown firmware %q", spec.Firmware)
	}

	cdromBoot, diskBoot := 2, 1
	if spec.ISOPath != "" {
		cdromBoot, diskBoot = 1, 2
	}

	bootDisk := diskDeviceXML(spec.DiskPath, BootDiskTarget)
	bootDisk.Boot = &bootXML{Order: diskBoot}
	d.Devices.Disks = []diskXML{bootDisk, cdromXML(spec.ISOPath, cdromBoot)}

	// the root complex comes first, as index 0, or libvirt takes the first
	// port for it
	d.Devices.Controllers = []controllerXML{{Type: "pci", Index: "0", Model: "pcie-root"}}
	for range pcieRootPorts {
		d.Devices.Controllers = append(d.Devices.Controllers, controllerXML{Type: "pci", Model: "pcie-root-port"})
	}

	nic, err := interfaceXMLOf(spec.NIC)
	if err != nil {
		return "", err
	}
	d.Devices.Interfaces = []interfaceXML{nic}

	d.Devices.Graphics = []graphicsXML{{Type: "vnc", Listen: &graphicsListenXML{Type: "socket"}}}
	d.Devices.Video = &videoXML{Model: modelXML{Type: "virtio"}}
	d.Devices.Inputs = []inputXML{{Type: "tablet", Bus: "usb"}, {Type: "keyboard", Bus: "usb"}}
	d.Devices.RNG = &rngXML{Model: "virtio", Backend: rngBackendXML{Model: "random", Value: "/dev/urandom"}}
	d.Devices.Balloon = &balloonXML{Model: "virtio", Stats: balloonStatsXML{Period: balloonStatsPeriod}}

	out, err := xml.MarshalIndent(d, "", "  ")
	if err != nil {
		return "", err
	}

	return string(out), nil
}

// diskDeviceXML is a qcow2 disk on virtio, with writeback cache (O_DIRECT
// fails on mergerfs).
func diskDeviceXML(path, target string) diskXML {
	return diskXML{
		Type:   "file",
		Device: "disk",
		Driver: &diskDriverXML{Name: "qemu", Type: "qcow2", Cache: "writeback", Discard: "unmap"},
		Source: &diskSourceXML{File: path},
		Target: diskTargetXML{Dev: target, Bus: "virtio"},
	}
}

// BuildDiskXML writes a disk for DomainAttachDeviceFlags and
// DomainDetachDeviceFlags.
func BuildDiskXML(path, target string) (string, error) {
	out, err := xml.Marshal(diskDeviceXML(path, target))
	if err != nil {
		return "", err
	}

	return string(out), nil
}

// metadataSetXML is Metadata for DomainSetMetadata, without a namespace:
// libvirt puts the element in the one it is given along with it, and refuses
// an element that declares its own.
type metadataSetXML struct {
	XMLName   xml.Name   `xml:"vm"`
	Volumes   []string   `xml:"volume"`
	Firmware  string     `xml:"firmware"`
	Ports     []Port     `xml:"port"`
	OS        string     `xml:"os,omitempty"`
	Snapshots []Snapshot `xml:"snapshot"`
}

// interfaceXMLOf is a virtio NIC where n says. macvtap is in mode bridge, so
// VMs on the same card reach each other.
func interfaceXMLOf(n NIC) (interfaceXML, error) {
	if n.Source == "" {
		return interfaceXML{}, errors.New("the network card has no source")
	}

	nic := interfaceXML{Type: n.Type, Model: &modelXML{Type: "virtio"}}
	switch n.Type {
	case NICNetwork:
		nic.Source = interfaceSourceXML{Network: n.Source}
	case NICBridge:
		nic.Source = interfaceSourceXML{Bridge: n.Source}
	case NICDirect:
		nic.Source = interfaceSourceXML{Dev: n.Source, Mode: "bridge"}
	default:
		return interfaceXML{}, fmt.Errorf("unknown network card type %q", n.Type)
	}
	if n.MAC != "" {
		nic.MAC = &macXML{Address: n.MAC}
	}

	return nic, nil
}

// BuildInterfaceXML writes a VM's NIC as n says, keeping its MAC, for
// DomainUpdateDeviceFlags: libvirt finds the NIC to change by its MAC.
func BuildInterfaceXML(n NIC) (string, error) {
	nic, err := interfaceXMLOf(n)
	if err != nil {
		return "", err
	}

	out, err := xml.Marshal(nic)
	if err != nil {
		return "", err
	}

	return string(out), nil
}

// BuildMetadataXML writes CasaOS's <metadata> element for DomainSetMetadata.
func BuildMetadataXML(m Metadata) (string, error) {
	out, err := xml.Marshal(metadataSetXML{Volumes: m.Volumes, Firmware: m.Firmware, Ports: m.Ports, OS: m.OS, Snapshots: m.Snapshots})
	if err != nil {
		return "", err
	}

	return string(out), nil
}

func cdromXML(isoPath string, bootOrder int) diskXML {
	disk := diskXML{
		Type:     "file",
		Device:   "cdrom",
		Driver:   &diskDriverXML{Name: "qemu", Type: "raw"},
		Target:   diskTargetXML{Dev: CDROMTarget, Bus: "sata"},
		ReadOnly: &struct{}{},
	}

	// The ISO is the user's file, anywhere under /DATA, and possibly used by
	// several VMs: libvirt must not chown it to the QEMU user, which it would
	// not undo for a read-only disk. QEMU reads it as it is, so it must be
	// world-readable, as files in /DATA are.
	if isoPath != "" {
		disk.Source = &diskSourceXML{File: isoPath, Seclabel: &seclabelXML{Model: "dac", Relabel: "no"}}
	}

	if bootOrder > 0 {
		disk.Boot = &bootXML{Order: bootOrder}
	}

	return disk
}

// BuildCDROMXML writes the CD-ROM drive holding isoPath, or empty, for
// DomainUpdateDeviceFlags. The boot order is not repeated: libvirt refuses to
// change it on a live device, and keeps the one defined.
func BuildCDROMXML(isoPath string, bootOrder int) (string, error) {
	out, err := xml.Marshal(cdromXML(isoPath, bootOrder))
	if err != nil {
		return "", err
	}

	return string(out), nil
}

// Disk is a file-backed disk of a domain.
type Disk struct {
	Path   string
	Target string // the device the guest sees: vda, sdb, …
}

// USBDevice is a host USB device given to a VM, by vendor and product: it
// stays the VM's when unplugged and plugged in again, wherever it goes.
type USBDevice struct {
	Vendor  string // 4 hex digits, no 0x
	Product string
}

var hexID = regexp.MustCompile(`^[0-9a-f]{4}$`)

// Valid reports whether both ids are 4 hex digits.
func (u USBDevice) Valid() bool { return hexID.MatchString(u.Vendor) && hexID.MatchString(u.Product) }

type hostdevXML struct {
	XMLName xml.Name         `xml:"hostdev"`
	Mode    string           `xml:"mode,attr"`
	Type    string           `xml:"type,attr"`
	Managed string           `xml:"managed,attr,omitempty"`
	Source  hostdevSourceXML `xml:"source"`
}

type hostdevSourceXML struct {
	// optional: the VM starts without the device when it is not plugged in
	StartupPolicy string  `xml:"startupPolicy,attr,omitempty"`
	Vendor        *hexXML `xml:"vendor"`
	Product       *hexXML `xml:"product"`
}

type hexXML struct {
	ID string `xml:"id,attr"`
}

// BuildUSBHostdevXML writes u as a device to attach or detach.
func BuildUSBHostdevXML(u USBDevice) (string, error) {
	if !u.Valid() {
		return "", fmt.Errorf("USB device %q:%q is not vendor:product in hex", u.Vendor, u.Product)
	}

	out, err := xml.Marshal(hostdevXML{
		Mode:    "subsystem",
		Type:    "usb",
		Managed: "yes",
		Source: hostdevSourceXML{
			StartupPolicy: "optional",
			Vendor:        &hexXML{ID: "0x" + u.Vendor},
			Product:       &hexXML{ID: "0x" + u.Product},
		},
	})
	return string(out), err
}

func hexIDOf(h *hexXML) string {
	if h == nil {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(h.ID, "0x"), "0X"))
}

// Interface is a NIC of a domain.
type Interface struct {
	Type    string // network, bridge, direct (macvtap), …
	Network string // for type network
	Source  string // the host bridge for type bridge, the host card for type direct
	MAC     string
}

// Domain is what the service reads back from a domain's XML.
type Domain struct {
	Name      string
	UUID      string
	Title     string
	VCPUs     int
	MemoryMiB int
	UEFI      bool
	NVRAM     string   // the file with the UEFI variables; empty before the first start
	Disks     []Disk   // file-backed disks, in order; the first is the one the VM boots from
	CDROM     string   // the ISO in the first CD-ROM drive; empty when there is none
	CDROMBoot int      // the first CD-ROM drive's boot order; 0: none
	Targets   []string // every disk and CD-ROM target, with a file or not
	NICs      []Interface
	USB       []USBDevice // host USB devices given to the VM
	HasCDROM  bool
	VNC       bool
	VNCSocket string // only in a running domain's live XML
	Metadata  *Metadata

	// Description is the owner's free text about the VM (virsh desc).
	Description string
}

type domainInXML struct {
	Name   string `xml:"name"`
	UUID   string `xml:"uuid"`
	Title  string `xml:"title"`
	Memory struct {
		Unit  string `xml:"unit,attr"`
		Value uint64 `xml:",chardata"`
	} `xml:"memory"`
	VCPU struct {
		Current int `xml:"current,attr"`
		Value   int `xml:",chardata"`
	} `xml:"vcpu"`
	OS struct {
		Firmware string `xml:"firmware,attr"`
		Loader   *struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"loader"`
		NVRAM *struct {
			Value string `xml:",chardata"`
		} `xml:"nvram"`
	} `xml:"os"`
	Metadata struct {
		VM *Metadata `xml:"https://recasaos.github.io/xmlns/vm/1 vm"`
	} `xml:"metadata"`
	Devices struct {
		Disks []struct {
			Type   string `xml:"type,attr"`
			Device string `xml:"device,attr"`
			Source *struct {
				File string `xml:"file,attr"`
			} `xml:"source"`
			Target struct {
				Dev string `xml:"dev,attr"`
			} `xml:"target"`
			Boot *bootXML `xml:"boot"`
		} `xml:"disk"`
		Graphics   []graphicsXML  `xml:"graphics"`
		Interfaces []interfaceXML `xml:"interface"`
		Hostdevs   []hostdevXML   `xml:"hostdev"`
	} `xml:"devices"`

	Description string `xml:"description"`
}

// ParseDomainXML reads what the service needs from a domain's XML.
func ParseDomainXML(s string) (Domain, error) {
	var in domainInXML
	if err := xml.Unmarshal([]byte(s), &in); err != nil {
		return Domain{}, err
	}

	vcpus := in.VCPU.Value
	if in.VCPU.Current > 0 {
		vcpus = in.VCPU.Current
	}

	d := Domain{
		Name:      in.Name,
		UUID:      in.UUID,
		Title:     in.Title,
		VCPUs:     vcpus,
		MemoryMiB: int(toKiB(in.Memory.Value, in.Memory.Unit) / 1024),
		UEFI:      in.OS.Firmware == "efi" || (in.OS.Loader != nil && in.OS.Loader.Type == "pflash"),
		Metadata:  in.Metadata.VM,
	}
	d.Description = in.Description
	if in.OS.NVRAM != nil {
		d.NVRAM = strings.TrimSpace(in.OS.NVRAM.Value)
	}

	for _, disk := range in.Devices.Disks {
		if disk.Target.Dev != "" {
			d.Targets = append(d.Targets, disk.Target.Dev)
		}

		if disk.Type != "file" {
			continue
		}

		file := ""
		if disk.Source != nil {
			file = disk.Source.File
		}

		switch disk.Device {
		case "disk":
			if file != "" {
				d.Disks = append(d.Disks, Disk{Path: file, Target: disk.Target.Dev})
			}
		case "cdrom":
			if !d.HasCDROM {
				d.HasCDROM = true
				d.CDROM = file
				if disk.Boot != nil {
					d.CDROMBoot = disk.Boot.Order
				}
			}
		}
	}

	for _, nic := range in.Devices.Interfaces {
		// the live XML of a NIC on a network names its bridge too: only a
		// bridge NIC's bridge is its source
		i := Interface{Type: nic.Type, Network: nic.Source.Network}
		switch nic.Type {
		case NICBridge:
			i.Source = nic.Source.Bridge
		case NICDirect:
			i.Source = nic.Source.Dev
		}
		if nic.MAC != nil {
			i.MAC = nic.MAC.Address
		}
		d.NICs = append(d.NICs, i)
	}

	for _, h := range in.Devices.Hostdevs {
		if h.Type != "usb" {
			continue
		}
		u := USBDevice{Vendor: hexIDOf(h.Source.Vendor), Product: hexIDOf(h.Source.Product)}
		if u.Valid() {
			d.USB = append(d.USB, u)
		}
	}

	for _, g := range in.Devices.Graphics {
		if g.Type != "vnc" {
			continue
		}

		d.VNC = true
		switch {
		case g.Socket != "":
			d.VNCSocket = g.Socket
		case g.Listen != nil && g.Listen.Type == "socket":
			d.VNCSocket = g.Listen.Socket
		}
		break
	}

	return d, nil
}

// DiskByTarget returns the file-backed disk at target.
func (d Domain) DiskByTarget(target string) (Disk, bool) {
	for _, disk := range d.Disks {
		if disk.Target == target {
			return disk, true
		}
	}

	return Disk{}, false
}

// NextDiskTarget returns the first virtio target no device uses, vdb to vdz.
func (d Domain) NextDiskTarget() (string, error) {
	for c := 'b'; c <= 'z'; c++ {
		target := "vd" + string(c)
		if !slices.Contains(d.Targets, target) {
			return target, nil
		}
	}

	return "", errors.New("the VM has no free disk target left")
}

// toKiB converts a libvirt memory value, KiB unless unit says otherwise.
func toKiB(value uint64, unit string) uint64 {
	switch strings.ToLower(unit) {
	case "b", "bytes":
		return value / 1024
	case "m", "mib":
		return value * 1024
	case "g", "gib":
		return value * 1024 * 1024
	default: // "", "k", "KiB"
		return value
	}
}
