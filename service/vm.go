package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/firewall"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const gib = int64(1) << 30

// VMs runs the VM operations. libvirt is the only state, apart from the VMs
// whose disk is still being imported.
type VMs struct {
	conn   *libvirt.Conn
	events *Events

	// net is the CasaOS network new VMs are attached to, whose firewall
	// rules fw writes; fwMu keeps two writes from interleaving
	net  libvirt.NetworkSpec
	fw   *firewall.Firewall
	fwMu sync.Mutex

	// ctx ends background imports when the service stops.
	ctx context.Context

	mu       sync.Mutex
	creating map[string]bool // domain UUIDs whose disk is being imported

	// Downloads fetches images from URLs the user gives into the iso folder.
	Downloads *Downloads
}

func NewVMs(ctx context.Context, conn *libvirt.Conn, events *Events, net libvirt.NetworkSpec, fw *firewall.Firewall) *VMs {
	downloads := NewDownloads(ctx, func() string { return filepath.Join(config.StoragePath(), ISOFolderName) })
	return &VMs{conn: conn, events: events, ctx: ctx, creating: map[string]bool{}, net: net, fw: fw, Downloads: downloads}
}

// Network is the CasaOS network.
func (s *VMs) Network() libvirt.NetworkSpec {
	return s.net
}

func (s *VMs) isCreating(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.creating[id]
}

func (s *VMs) setCreating(id string, creating bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if creating {
		s.creating[id] = true
	} else {
		delete(s.creating, id)
	}
}

func uuidString(u golibvirt.UUID) string {
	return uuid.UUID(u).String()
}

func titleOf(d libvirt.Domain) string {
	if d.Title != "" {
		return d.Title
	}

	return d.Name
}

// List returns every domain on the host.
func (s *VMs) List() ([]codegen.VM, error) {
	l, err := s.conn.Get()
	if err != nil {
		return nil, err
	}

	domains, _, err := l.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, err
	}

	vms := make([]codegen.VM, 0, len(domains))
	for _, dom := range domains {
		vm, err := s.toVM(l, dom)
		if err != nil {
			if libvirt.IsNotFound(err) { // undefined while listing
				continue
			}
			return nil, err
		}
		vms = append(vms, vm)
	}

	slices.SortFunc(vms, func(a, b codegen.VM) int {
		return strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
	})

	return vms, nil
}

// Get returns one VM.
func (s *VMs) Get(id uuid.UUID) (codegen.VM, error) {
	l, dom, err := s.lookup(id)
	if err != nil {
		return codegen.VM{}, err
	}

	return s.toVM(l, dom)
}

func (s *VMs) lookup(id uuid.UUID) (*golibvirt.Libvirt, golibvirt.Domain, error) {
	l, err := s.conn.Get()
	if err != nil {
		return nil, golibvirt.Domain{}, err
	}

	dom, err := l.DomainLookupByUUID(golibvirt.UUID(id))
	if libvirt.IsNotFound(err) {
		return nil, dom, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	return l, dom, err
}

func (s *VMs) state(l *golibvirt.Libvirt, dom golibvirt.Domain) (string, error) {
	if s.isCreating(uuidString(dom.UUID)) {
		return libvirt.StateCreating, nil
	}

	state, _, err := l.DomainGetState(dom, 0)
	if err != nil {
		return "", err
	}

	return libvirt.StateName(golibvirt.DomainState(state)), nil
}

func (s *VMs) toVM(l *golibvirt.Libvirt, dom golibvirt.Domain) (codegen.VM, error) {
	state, err := s.state(l, dom)
	if err != nil {
		return codegen.VM{}, err
	}

	xmlDesc, err := l.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return codegen.VM{}, err
	}

	d, err := libvirt.ParseDomainXML(xmlDesc)
	if err != nil {
		return codegen.VM{}, err
	}

	// the definition the next start will use, which PATCH changes
	next := d
	if state != libvirt.StateShutOff && state != libvirt.StateCreating {
		if inactive, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive); err == nil {
			if parsed, err := libvirt.ParseDomainXML(inactive); err == nil {
				next = parsed
			}
		}
	}

	firmware := codegen.Bios
	if d.UEFI {
		firmware = codegen.Uefi
	}

	managed := d.Metadata != nil
	pendingRestart := next.VCPUs != d.VCPUs || next.MemoryMiB != d.MemoryMiB || !slices.Equal(next.Disks, d.Disks) ||
		!slices.Equal(next.NICs, d.NICs)

	// the disks of the next start, so that one added while the VM runs, but
	// not plugged in, is listed too
	disks := make([]codegen.Disk, 0, len(next.Disks))
	for i, dd := range next.Disks {
		boot := i == 0
		disk := codegen.Disk{Path: dd.Path, Target: dd.Target, Boot: &boot, Managed: s.ownsDisk(next, dd.Path)}

		if vol, err := l.StorageVolLookupByPath(dd.Path); err == nil {
			if _, capacity, allocation, err := l.StorageVolGetInfo(vol); err == nil {
				c, a := int64(capacity), int64(allocation)
				disk.CapacityBytes, disk.AllocationBytes = &c, &a
			}
		}

		disks = append(disks, disk)
	}

	network, ports := s.networkOf(l, next)

	// without a fixed address, the lease says where the VM is while it runs
	if network != nil && network.Casaos && network.Ip == nil && state == libvirt.StateRunning && network.Mac != nil {
		if ip := leaseAddress(l, dom, *network.Mac); ip != "" {
			network.Ip = &ip
		}
	}

	// on a bridge the host only learns the VM's address from its traffic: the
	// ARP table has it while the VM runs, if it has not changed network since
	if network != nil && network.Mode != nil && *network.Mode == codegen.VMNetworkModeBridge &&
		state == libvirt.StateRunning && len(d.NICs) > 0 && len(next.NICs) > 0 && d.NICs[0] == next.NICs[0] {
		if ip := arpAddress(l, dom, d.NICs[0].MAC); ip != "" {
			network.Ip = &ip
		}
	}

	var osName codegen.OS
	snapshots := []codegen.Snapshot{}
	if next.Metadata != nil {
		osName = codegen.OS(next.Metadata.OS)
		for _, sn := range next.Metadata.Snapshots {
			created, _ := time.Parse(time.RFC3339, sn.Created)
			snapshots = append(snapshots, codegen.Snapshot{Id: sn.ID, Name: sn.Name, CreatedAt: created})
		}
	}

	usb := usbOf(next, hostUSB())

	return codegen.VM{
		Usb:            &usb,
		Os:             &osName,
		Snapshots:      &snapshots,
		Network:        network,
		Ports:          &ports,
		Id:             uuid.UUID(dom.UUID),
		Name:           d.Name,
		Title:          titleOf(d),
		State:          codegen.VMState(state),
		Vcpus:          &next.VCPUs,
		MemoryMib:      &next.MemoryMiB,
		Firmware:       &firmware,
		Cdrom:          &d.CDROM,
		Disks:          &disks,
		Managed:        managed,
		Console:        &d.VNC,
		PendingRestart: &pendingRestart,
	}, nil
}

// networkOf describes d's first NIC and its port forwards.
func (s *VMs) networkOf(l *golibvirt.Libvirt, d libvirt.Domain) (*codegen.VMNetwork, []codegen.PortForward) {
	ports := []codegen.PortForward{}
	if d.Metadata != nil {
		for _, p := range d.Metadata.Ports {
			ports = append(ports, codegen.PortForward{Protocol: codegen.PortForwardProtocol(p.Protocol), HostPort: p.Host, GuestPort: p.Guest})
		}
	}

	if len(d.NICs) == 0 {
		return nil, ports
	}

	nic := d.NICs[0]
	network := &codegen.VMNetwork{Type: nic.Type}
	if nic.Network != "" {
		network.Name = &nic.Network
	}
	if nic.Source != "" {
		network.Source = &nic.Source
	}
	if nic.MAC != "" {
		network.Mac = &nic.MAC
	}

	var mode codegen.VMNetworkMode
	switch nic.Type {
	case libvirt.NICBridge:
		mode = codegen.VMNetworkModeBridge
	case libvirt.NICDirect:
		mode = codegen.VMNetworkModeMacvtap
	}
	if mode != "" {
		network.Mode = &mode
	}

	if mac, ok := s.onCasaOS(d); ok {
		network.Casaos = true
		mode = codegen.VMNetworkModeCasaos
		network.Mode = &mode
		if _, info, err := s.casaosNetwork(l); err == nil {
			if ip := info.IPOf(mac); ip != "" {
				network.Ip = &ip
				fixed := true
				network.Fixed = &fixed
			}
		}
	}

	return network, ports
}

// ownsDisk reports whether path is a disk CasaOS created for d: recorded in
// its metadata and in the storage folder.
func (s *VMs) ownsDisk(d libvirt.Domain, path string) bool {
	return d.Metadata != nil &&
		slices.Contains(d.Metadata.Volumes, path) &&
		underStorageRoots(filepath.Clean(path))
}

func libvirtArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}

	return "x86_64"
}

// newName picks an unused 8-hex-digit domain name, which also names the disk.
func newName(l *golibvirt.Libvirt, storagePath string) (string, error) {
	for range 10 {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		name := hex.EncodeToString(b)

		if _, err := l.DomainLookupByName(name); !libvirt.IsNotFound(err) {
			continue
		}

		if _, err := os.Stat(filepath.Join(storagePath, name+".qcow2")); !os.IsNotExist(err) {
			continue
		}

		return name, nil
	}

	return "", errors.New("could not find a free VM name")
}

// Create defines a VM. It returns true when the disk is being imported in the
// background, and the VM is in state creating.
func (s *VMs) Create(req codegen.VMCreate) (codegen.VM, bool, error) {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return codegen.VM{}, false, fmt.Errorf("%w: the title is empty", ErrInvalid)
	}

	var (
		img    Image
		hasImg bool
	)
	if req.ImagePath != nil && *req.ImagePath != "" {
		var err error
		if img, err = ValidateImagePath(*req.ImagePath); err != nil {
			return codegen.VM{}, false, err
		}
		hasImg = true
	}

	l, err := s.conn.Get()
	if err != nil {
		return codegen.VM{}, false, err
	}

	host, err := libvirt.GetHost(l)
	if err != nil {
		return codegen.VM{}, false, err
	}

	if req.Vcpus > host.CPUs {
		return codegen.VM{}, false, fmt.Errorf("%w: %d vCPUs asked, the host has %d", ErrInvalid, req.Vcpus, host.CPUs)
	}

	if req.MemoryMib > host.MemoryMiB {
		return codegen.VM{}, false, fmt.Errorf("%w: %d MiB of memory asked, the host has %d", ErrInvalid, req.MemoryMib, host.MemoryMiB)
	}

	firmware := libvirt.FirmwareBIOS
	if host.UEFI {
		firmware = libvirt.FirmwareUEFI
	}
	if req.Firmware != nil {
		firmware = string(*req.Firmware)
	}
	if firmware == libvirt.FirmwareUEFI && !host.UEFI {
		return codegen.VM{}, false, fmt.Errorf("%w: UEFI firmware is not installed on this host (package ovmf)", ErrInvalid)
	}

	storagePath := config.StoragePath()
	if req.StoragePath != nil && strings.TrimSpace(*req.StoragePath) != "" {
		if storagePath, err = ValidateStorageFolder(strings.TrimSpace(*req.StoragePath)); err != nil {
			return codegen.VM{}, false, err
		}
	}

	pool, err := libvirt.EnsurePool(l, storagePath)
	if err != nil {
		return codegen.VM{}, false, err
	}

	importing := hasImg && !img.ISO
	diskBytes := int64(req.DiskGib) * gib

	free, err := freeBytes(storagePath)
	if err != nil {
		return codegen.VM{}, false, err
	}
	// an empty qcow2 takes almost nothing until the guest writes; an import
	// needs at least the image's own size
	if importing && free < img.Size {
		return codegen.VM{}, false, fmt.Errorf("%w: %s has %d MiB free, the image is %d MiB", ErrInvalid, storagePath, free>>20, img.Size>>20)
	}

	name, err := newName(l, storagePath)
	if err != nil {
		return codegen.VM{}, false, err
	}

	diskPath := filepath.Join(storagePath, name+".qcow2")

	// the CasaOS network is made in any case: its reservations are where MACs
	// are picked from
	if _, err := libvirt.EnsureNetwork(l, s.net); err != nil {
		return codegen.VM{}, false, err
	}

	mac, err := s.newMAC(l)
	if err != nil {
		return codegen.VM{}, false, err
	}

	nic, err := s.nicFor(req.Network, mac)
	if err != nil {
		return codegen.VM{}, false, err
	}

	// on the CasaOS network, a fixed address is reserved for its MAC before
	// the VM exists, so its first DHCP request gets it, unless DHCP was asked
	// for; release is then a no-op
	if nic.Type == libvirt.NICNetwork {
		if _, err := s.setAddress(l, mac, addressOf(req.Network)); err != nil {
			return codegen.VM{}, false, err
		}
	}

	spec := libvirt.DomainSpec{
		Name:      name,
		Title:     title,
		VCPUs:     req.Vcpus,
		MemoryMiB: req.MemoryMib,
		Firmware:  firmware,
		DiskPath:  diskPath,
		NIC:       nic,
		Arch:      libvirtArch(),
		OS:        osOf(req.Os),
	}
	if hasImg && img.ISO {
		spec.ISOPath = img.Path
	}

	domXML, err := libvirt.BuildDomainXML(spec)
	if err != nil {
		s.release(l, mac)
		return codegen.VM{}, false, fmt.Errorf("%w: %s", ErrInvalid, err)
	}

	var vol golibvirt.StorageVol
	if !importing {
		volXML, err := libvirt.BuildVolumeXML(name+".qcow2", diskBytes)
		if err != nil {
			return codegen.VM{}, false, err
		}

		if vol, err = l.StorageVolCreateXML(pool, volXML, 0); err != nil {
			s.release(l, mac)
			return codegen.VM{}, false, fmt.Errorf("create disk: %w", err)
		}
	}

	dom, err := l.DomainDefineXMLFlags(domXML, golibvirt.DomainDefineValidate)
	if err != nil {
		if !importing {
			if err := l.StorageVolDelete(vol, 0); err != nil {
				logger.Error("error when removing the disk of a VM that could not be defined", zap.String("path", diskPath), zap.Error(err))
			}
		}
		s.release(l, mac)
		return codegen.VM{}, false, fmt.Errorf("define VM: %w", err)
	}

	id := uuidString(dom.UUID)
	start := req.Start == nil || *req.Start

	if nic.Type == libvirt.NICBridge {
		s.applyFirewall(l)
	}

	if importing {
		s.setCreating(id, true)
		go s.finishImport(l, pool, dom, title, img, diskPath, diskBytes, mac, start)

		vm, err := s.toVM(l, dom)
		return vm, true, err
	}

	s.events.Publish(EventCreateEnd, map[string]string{PropertyID: id, PropertyName: name, PropertyTitle: title})

	if start {
		if err := l.DomainCreate(dom); err != nil {
			// the VM exists; say why it did not start
			vm, _ := s.toVM(l, dom)
			return vm, false, fmt.Errorf("%w: the VM was created but did not start: %s", ErrConflict, err)
		}
	}

	vm, err := s.toVM(l, dom)
	return vm, false, err
}

func (s *VMs) finishImport(l *golibvirt.Libvirt, pool golibvirt.StoragePool, dom golibvirt.Domain, title string, img Image, diskPath string, diskBytes int64, mac string, start bool) {
	id := uuidString(dom.UUID)
	properties := map[string]string{PropertyID: id, PropertyName: dom.Name, PropertyTitle: title}

	logger.Info("importing a disk image", zap.String("vm", dom.Name), zap.String("image", img.Path))

	err := importDisk(s.ctx, img, diskPath, diskBytes)
	if err == nil {
		err = l.StoragePoolRefresh(pool, 0)
	}

	s.setCreating(id, false)

	if err != nil {
		logger.Error("error when importing a disk image, removing the VM", zap.String("vm", dom.Name), zap.String("image", img.Path), zap.Error(err))

		if err := l.DomainUndefineFlags(dom, golibvirt.DomainUndefineNvram|golibvirt.DomainUndefineManagedSave); err != nil {
			logger.Error("error when removing a VM whose import failed", zap.String("vm", dom.Name), zap.Error(err))
		}
		os.Remove(diskPath)
		s.release(l, mac)

		properties[PropertyMessage] = err.Error()
		s.events.Publish(EventCreateError, properties)
		return
	}

	logger.Info("imported a disk image", zap.String("vm", dom.Name))
	s.events.Publish(EventCreateEnd, properties)

	if start {
		if err := l.DomainCreate(dom); err != nil {
			logger.Error("error when starting an imported VM", zap.String("vm", dom.Name), zap.Error(err))
		}
	}
}

// Update changes the title at once, and vCPUs and memory in the definition,
// for the next start.
func (s *VMs) Update(id uuid.UUID, req codegen.VMUpdate) (codegen.VM, error) {
	l, dom, err := s.lookup(id)
	if err != nil {
		return codegen.VM{}, err
	}

	state, err := s.state(l, dom)
	if err != nil {
		return codegen.VM{}, err
	}
	if state == libvirt.StateCreating {
		return codegen.VM{}, fmt.Errorf("%w: the VM is still being created", ErrConflict)
	}

	live := state != libvirt.StateShutOff && state != libvirt.StateCrashed

	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		if title == "" {
			return codegen.VM{}, fmt.Errorf("%w: the title is empty", ErrInvalid)
		}

		flags := golibvirt.DomainAffectConfig
		if live {
			flags |= golibvirt.DomainAffectLive
		}

		if err := l.DomainSetMetadata(dom, int32(golibvirt.DomainMetadataTitle), golibvirt.OptString{title}, nil, nil, flags); err != nil {
			return codegen.VM{}, fmt.Errorf("set title: %w", err)
		}
	}

	if req.Os != nil {
		if err := s.setOS(l, dom, string(*req.Os), live); err != nil {
			return codegen.VM{}, err
		}
	}

	if req.Network != nil {
		if err := s.moveNetwork(l, dom, req.Network, live); err != nil {
			return codegen.VM{}, err
		}
		// its forwards, or its bridge, count from now on
		s.applyFirewall(l)
	}

	if req.Vcpus != nil || req.MemoryMib != nil {
		host, err := libvirt.GetHost(l)
		if err != nil {
			return codegen.VM{}, err
		}

		if req.Vcpus != nil {
			if *req.Vcpus > host.CPUs {
				return codegen.VM{}, fmt.Errorf("%w: %d vCPUs asked, the host has %d", ErrInvalid, *req.Vcpus, host.CPUs)
			}
			if err := setVCPUs(l, dom, *req.Vcpus); err != nil {
				return codegen.VM{}, fmt.Errorf("set vCPUs: %w", err)
			}
		}

		if req.MemoryMib != nil {
			if *req.MemoryMib > host.MemoryMiB {
				return codegen.VM{}, fmt.Errorf("%w: %d MiB of memory asked, the host has %d", ErrInvalid, *req.MemoryMib, host.MemoryMiB)
			}
			if err := setMemory(l, dom, *req.MemoryMib); err != nil {
				return codegen.VM{}, fmt.Errorf("set memory: %w", err)
			}
		}
	}

	return s.toVM(l, dom)
}

// osOf is the OS a request names; empty when it names none.
func osOf(o *codegen.OS) string {
	if o == nil {
		return ""
	}
	return string(*o)
}

// setOS records what the VM runs, for its icon.
func (s *VMs) setOS(l *golibvirt.Libvirt, dom golibvirt.Domain, name string, live bool) error {
	desc, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
	if err != nil {
		return err
	}

	d, err := libvirt.ParseDomainXML(desc)
	if err != nil {
		return err
	}

	if d.Metadata == nil {
		return fmt.Errorf("%w: CasaOS did not create this VM, so it keeps nothing about it", ErrConflict)
	}

	m := *d.Metadata
	m.OS = name
	return setMetadata(l, dom, m, live)
}

// moveNetwork puts a VM's first NIC where choice says, keeping its MAC; in
// the definition, for the next start. On the CasaOS network it gets a fixed
// address; leaving it, the VM gives the address and its port forwards back.
func (s *VMs) moveNetwork(l *golibvirt.Libvirt, dom golibvirt.Domain, choice *codegen.NetworkChoice, live bool) error {
	desc, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
	if err != nil {
		return err
	}

	d, err := libvirt.ParseDomainXML(desc)
	if err != nil {
		return err
	}

	if d.Metadata == nil {
		return fmt.Errorf("%w: CasaOS did not create this VM, so it does not change its network", ErrConflict)
	}
	if len(d.NICs) == 0 || d.NICs[0].MAC == "" {
		return fmt.Errorf("%w: the VM has no network card", ErrConflict)
	}

	mac := d.NICs[0].MAC
	nic, err := s.nicFor(choice, mac)
	if err != nil {
		return err
	}

	_, wasCasaOS := s.onCasaOS(d)
	toCasaOS := nic.Type == libvirt.NICNetwork

	// on the CasaOS network already: only the address may change, and at
	// once, since it lives in the network's DHCP
	if wasCasaOS && toCasaOS {
		fixed, err := s.setAddress(l, mac, addressOf(choice))
		if err != nil {
			return err
		}
		if !fixed {
			s.dropPorts(l, dom, d, live)
		}
		return nil
	}

	current := d.NICs[0]
	if current.Type == nic.Type && current.Source == nic.Source {
		return nil
	}

	if toCasaOS {
		if _, err := libvirt.EnsureNetwork(l, s.net); err != nil {
			return err
		}
		if _, err := s.setAddress(l, mac, addressOf(choice)); err != nil {
			return err
		}
	}

	nicXML, err := libvirt.BuildInterfaceXML(nic)
	if err != nil {
		return err
	}

	if err := l.DomainUpdateDeviceFlags(dom, nicXML, golibvirt.DomainDeviceModifyConfig); err != nil {
		if toCasaOS {
			s.release(l, mac)
		}
		return fmt.Errorf("change the network: %w", err)
	}

	// port forwards only exist on the CasaOS network
	if wasCasaOS && !toCasaOS {
		s.release(l, mac)
		s.dropPorts(l, dom, d, live)
	}

	logger.Info("changed a VM's network", zap.String("vm", d.Name), zap.String("mac", mac), zap.String("type", nic.Type), zap.String("source", nic.Source))
	return nil
}

// dropPorts removes a VM's port forwards, which need the CasaOS network and
// a fixed address there; the caller writes the rules again.
func (s *VMs) dropPorts(l *golibvirt.Libvirt, dom golibvirt.Domain, d libvirt.Domain, live bool) {
	if len(d.Metadata.Ports) == 0 {
		return
	}

	m := *d.Metadata
	m.Ports = nil
	if err := setMetadata(l, dom, m, live); err != nil {
		logger.Error("error when removing the port forwards of a VM", zap.String("vm", d.Name), zap.Error(err))
	}
}

// setVCPUs sets both the maximum and the current count in the definition, in
// the order libvirt accepts: the maximum can never be below the current one.
func setVCPUs(l *golibvirt.Libvirt, dom golibvirt.Domain, n int) error {
	current, err := l.DomainGetVcpusFlags(dom, uint32(golibvirt.DomainVCPUConfig|golibvirt.DomainVCPUMaximum))
	if err != nil {
		return err
	}

	setMax := func() error {
		return l.DomainSetVcpusFlags(dom, uint32(n), uint32(golibvirt.DomainVCPUConfig|golibvirt.DomainVCPUMaximum))
	}
	setCurrent := func() error {
		return l.DomainSetVcpusFlags(dom, uint32(n), uint32(golibvirt.DomainVCPUConfig))
	}

	if n > int(current) {
		if err := setMax(); err != nil {
			return err
		}
		return setCurrent()
	}

	if err := setCurrent(); err != nil {
		return err
	}
	return setMax()
}

// setMemory is setVCPUs for memory.
func setMemory(l *golibvirt.Libvirt, dom golibvirt.Domain, mib int) error {
	inactive, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
	if err != nil {
		return err
	}

	d, err := libvirt.ParseDomainXML(inactive)
	if err != nil {
		return err
	}

	kib := uint64(mib) * 1024
	setMax := func() error {
		return l.DomainSetMemoryFlags(dom, kib, uint32(golibvirt.DomainMemConfig|golibvirt.DomainMemMaximum))
	}
	setCurrent := func() error {
		return l.DomainSetMemoryFlags(dom, kib, uint32(golibvirt.DomainMemConfig))
	}

	if mib > d.MemoryMiB {
		if err := setMax(); err != nil {
			return err
		}
		return setCurrent()
	}

	if err := setCurrent(); err != nil {
		return err
	}
	return setMax()
}

// ChangeState runs a power action. libvirt refusing it for the VM's state
// (starting a running VM) is ErrConflict.
func (s *VMs) ChangeState(id uuid.UUID, action codegen.VMAction) (codegen.VM, error) {
	l, dom, err := s.lookup(id)
	if err != nil {
		return codegen.VM{}, err
	}

	if s.isCreating(uuidString(dom.UUID)) {
		return codegen.VM{}, fmt.Errorf("%w: the VM is still being created", ErrConflict)
	}

	switch action {
	case codegen.Start:
		err = l.DomainCreate(dom)
	case codegen.Shutdown:
		err = l.DomainShutdown(dom)
	case codegen.Reboot:
		err = l.DomainReboot(dom, golibvirt.DomainRebootDefault)
	case codegen.ForceOff:
		err = l.DomainDestroy(dom)
	case codegen.Reset:
		err = l.DomainReset(dom, 0)
	case codegen.Pause:
		err = l.DomainSuspend(dom)
	case codegen.Resume:
		err = l.DomainResume(dom)
	default:
		return codegen.VM{}, fmt.Errorf("%w: unknown action %q", ErrInvalid, action)
	}

	if err != nil {
		if libvirt.IsOperationInvalid(err) {
			return codegen.VM{}, fmt.Errorf("%w: %s", ErrConflict, err)
		}
		return codegen.VM{}, err
	}

	return s.toVM(l, dom)
}

// SetCDROM inserts isoPath into the VM's CD-ROM drive, or ejects it when
// isoPath is empty; live when the VM runs, and in the definition.
func (s *VMs) SetCDROM(id uuid.UUID, isoPath string) (codegen.VM, error) {
	if isoPath != "" {
		img, err := ValidateImagePath(isoPath)
		if err != nil {
			return codegen.VM{}, err
		}
		if !img.ISO {
			return codegen.VM{}, fmt.Errorf("%w: only an ISO can go in the CD-ROM drive", ErrInvalid)
		}
		isoPath = img.Path
	}

	l, dom, err := s.lookup(id)
	if err != nil {
		return codegen.VM{}, err
	}

	state, err := s.state(l, dom)
	if err != nil {
		return codegen.VM{}, err
	}
	if state == libvirt.StateCreating {
		return codegen.VM{}, fmt.Errorf("%w: the VM is still being created", ErrConflict)
	}

	xmlDesc, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
	if err != nil {
		return codegen.VM{}, err
	}

	d, err := libvirt.ParseDomainXML(xmlDesc)
	if err != nil {
		return codegen.VM{}, err
	}

	if !d.HasCDROM {
		return codegen.VM{}, fmt.Errorf("%w: the VM has no CD-ROM drive", ErrConflict)
	}

	// the drive is replaced whole: without its boot order, the VM would no
	// longer start from the disc
	deviceXML, err := libvirt.BuildCDROMXML(isoPath, d.CDROMBoot)
	if err != nil {
		return codegen.VM{}, err
	}

	flags := golibvirt.DomainDeviceModifyConfig
	if state != libvirt.StateShutOff && state != libvirt.StateCrashed {
		flags |= golibvirt.DomainDeviceModifyLive
	}
	if isoPath == "" {
		// eject even when the guest has locked the tray
		flags |= golibvirt.DomainDeviceModifyForce
	}

	if err := l.DomainUpdateDeviceFlags(dom, deviceXML, flags); err != nil {
		if libvirt.IsOperationInvalid(err) {
			return codegen.VM{}, fmt.Errorf("%w: %s", ErrConflict, err)
		}
		return codegen.VM{}, err
	}

	return s.toVM(l, dom)
}

// Delete undefines a VM that is not running and, when deleteDisks is set, the
// disks CasaOS created for it. It never deletes anything else.
func (s *VMs) Delete(id uuid.UUID, deleteDisks bool) (codegen.VMDeleted, error) {
	deleted, kept := []string{}, []string{}
	result := codegen.VMDeleted{Deleted: &deleted, Kept: &kept}

	l, dom, err := s.lookup(id)
	if err != nil {
		return result, err
	}

	state, err := s.state(l, dom)
	if err != nil {
		return result, err
	}

	if state != libvirt.StateShutOff && state != libvirt.StateCrashed {
		return result, fmt.Errorf("%w: the VM is %s; force it off first", ErrConflict, state)
	}

	xmlDesc, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
	if err != nil {
		return result, err
	}

	d, err := libvirt.ParseDomainXML(xmlDesc)
	if err != nil {
		return result, err
	}

	// undefine first: if it fails, the disks are still there for the VM. A
	// driver without NVRAM or checkpoints (libvirt's test driver) refuses
	// their flags, and has nothing of them to remove.
	err = l.DomainUndefineFlags(dom, golibvirt.DomainUndefineNvram|golibvirt.DomainUndefineManagedSave|golibvirt.DomainUndefineSnapshotsMetadata|golibvirt.DomainUndefineCheckpointsMetadata)
	if libvirt.IsUnsupportedFlags(err) {
		err = l.DomainUndefineFlags(dom, golibvirt.DomainUndefineManagedSave|golibvirt.DomainUndefineSnapshotsMetadata)
	}
	if err != nil {
		return result, fmt.Errorf("undefine VM: %w", err)
	}

	for _, disk := range d.Disks {
		path := disk.Path
		if !deleteDisks || !s.ownsDisk(d, path) {
			kept = append(kept, path)
			continue
		}

		if err := deleteVolume(l, path); err != nil {
			logger.Error("error when deleting a VM disk", zap.String("path", path), zap.Error(err))
			kept = append(kept, path)
			continue
		}

		deleted = append(deleted, path)
	}

	removeSnapshotFiles(d)

	// its fixed address and its forwards, or its bridge's rule, go with it
	if mac, ok := s.onCasaOS(d); ok {
		s.release(l, mac)
		if d.Metadata != nil && len(d.Metadata.Ports) > 0 {
			s.applyFirewall(l)
		}
	} else if len(d.NICs) > 0 && d.NICs[0].Type == libvirt.NICBridge {
		s.applyFirewall(l)
	}

	s.events.Publish(EventDeleteEnd, map[string]string{
		PropertyID:           uuidString(dom.UUID),
		PropertyName:         d.Name,
		PropertyTitle:        titleOf(d),
		PropertyDisksDeleted: strings.Join(deleted, ","),
	})

	return result, nil
}

// deleteVolume deletes the pool volume at path; a file already gone is fine.
func deleteVolume(l *golibvirt.Libvirt, path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	vol, err := lookupVolume(l, path)
	if err != nil {
		return err
	}

	return l.StorageVolDelete(vol, golibvirt.StorageVolDeleteNormal)
}

// lookupVolume returns the pool volume at path, refreshing the pool once if
// libvirt does not know it yet.
func lookupVolume(l *golibvirt.Libvirt, path string) (golibvirt.StorageVol, error) {
	vol, err := l.StorageVolLookupByPath(path)
	if !libvirt.IsNotFound(err) {
		return vol, err
	}

	pool, err := libvirt.EnsurePool(l, filepath.Dir(path))
	if err != nil {
		return vol, err
	}
	if err := l.StoragePoolRefresh(pool, 0); err != nil {
		return vol, err
	}

	return l.StorageVolLookupByPath(path)
}

// ConsoleSocket is the VNC unix socket of a running VM.
func (s *VMs) ConsoleSocket(id uuid.UUID) (string, error) {
	l, dom, err := s.lookup(id)
	if err != nil {
		return "", err
	}

	state, err := s.state(l, dom)
	if err != nil {
		return "", err
	}

	if state != libvirt.StateRunning && state != libvirt.StatePaused {
		return "", fmt.Errorf("%w: the VM is %s", ErrConflict, state)
	}

	xmlDesc, err := l.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return "", err
	}

	d, err := libvirt.ParseDomainXML(xmlDesc)
	if err != nil {
		return "", err
	}

	if d.VNCSocket == "" {
		return "", fmt.Errorf("%w: the VM has no VNC display on a unix socket", ErrConflict)
	}

	return d.VNCSocket, nil
}
