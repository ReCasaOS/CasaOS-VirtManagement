package service

import (
	"fmt"
	"os"
	"slices"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/hostusb"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// hostUSB lists the host's USB devices now; a host without USB, or a read
// that fails, has none.
func hostUSB() []hostusb.Device {
	mounts, _ := os.ReadFile("/proc/mounts")

	devices, err := hostusb.List(hostusb.SysBusUSB, string(mounts))
	if err != nil {
		logger.Error("error when listing the host's USB devices", zap.Error(err))
		return nil
	}

	return devices
}

// usbOwner is the VM a USB device is given to.
type usbOwner struct {
	id    uuid.UUID
	title string
}

// usbOwners maps vendor:product to the VM whose definition has the device,
// among the VMs CasaOS created; except (uuid.Nil: none) is left out.
func usbOwners(l *golibvirt.Libvirt, except uuid.UUID) map[string]usbOwner {
	owners := map[string]usbOwner{}

	domains, _, err := l.ConnectListAllDomains(1, 0)
	if err != nil {
		return owners
	}

	for _, dom := range domains {
		if uuid.UUID(dom.UUID) == except {
			continue
		}

		desc, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
		if err != nil {
			continue
		}

		d, err := libvirt.ParseDomainXML(desc)
		if err != nil {
			continue
		}

		for _, u := range d.USB {
			owners[u.Vendor+":"+u.Product] = usbOwner{id: uuid.UUID(dom.UUID), title: titleOf(d)}
		}
	}

	return owners
}

// HostUSB lists the host's USB devices, with what the host uses each for and
// the VM each is given to.
func (s *VMs) HostUSB() ([]codegen.HostUSBDevice, error) {
	l, err := s.conn.Get()
	if err != nil {
		return nil, err
	}

	owners := usbOwners(l, uuid.Nil)

	out := []codegen.HostUSBDevice{}
	for _, d := range hostUSB() {
		bus, address := d.Bus, d.Address
		dev := codegen.HostUSBDevice{Vendor: d.Vendor, Product: d.Product, Name: d.Name, Bus: &bus, Address: &address}
		if d.InUse != "" {
			inUse := d.InUse
			dev.InUse = &inUse
		}
		if o, ok := owners[d.ID()]; ok {
			id, title := o.id, o.title
			dev.VmId, dev.VmTitle = &id, &title
		}
		out = append(out, dev)
	}

	return out, nil
}

// usbOf describes the USB devices of d's definition, named after the host's
// devices when they are plugged in.
func usbOf(d libvirt.Domain, host []hostusb.Device) []codegen.VMUSBDevice {
	out := make([]codegen.VMUSBDevice, 0, len(d.USB))
	for _, u := range d.USB {
		dev := codegen.VMUSBDevice{Vendor: u.Vendor, Product: u.Product}
		for _, h := range host {
			if h.Vendor == u.Vendor && h.Product == u.Product {
				name := h.Name
				dev.Name, dev.Connected = &name, true
				break
			}
		}
		out = append(out, dev)
	}

	return out
}

// SetUSB gives a VM exactly the devices asked for: new ones are added, and
// plugged in when the VM runs; those not asked for any more are taken back.
func (s *VMs) SetUSB(id uuid.UUID, wanted []codegen.USBID) (codegen.VM, error) {
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
	running := state == libvirt.StateRunning || state == libvirt.StatePaused

	desc, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
	if err != nil {
		return codegen.VM{}, err
	}

	d, err := libvirt.ParseDomainXML(desc)
	if err != nil {
		return codegen.VM{}, err
	}

	if d.Metadata == nil {
		return codegen.VM{}, fmt.Errorf("%w: CasaOS did not create this VM, so it does not change its devices", ErrConflict)
	}

	want := make([]libvirt.USBDevice, 0, len(wanted))
	for _, w := range wanted {
		u := libvirt.USBDevice{Vendor: w.Vendor, Product: w.Product}
		if !u.Valid() {
			return codegen.VM{}, fmt.Errorf("%w: %s:%s is not a USB vendor:product in hex", ErrInvalid, w.Vendor, w.Product)
		}
		if !slices.Contains(want, u) {
			want = append(want, u)
		}
	}

	// what runs now: a device is taken out of the live VM only if it is there
	live := d
	if running {
		if desc, err := l.DomainGetXMLDesc(dom, 0); err == nil {
			if parsed, err := libvirt.ParseDomainXML(desc); err == nil {
				live = parsed
			}
		}
	}

	var add []libvirt.USBDevice
	for _, u := range want {
		if !slices.Contains(d.USB, u) {
			add = append(add, u)
		}
	}

	if len(add) > 0 {
		host := hostUSB()
		owners := usbOwners(l, id)
		for _, u := range add {
			key := u.Vendor + ":" + u.Product
			i := slices.IndexFunc(host, func(h hostusb.Device) bool { return h.ID() == key })
			switch {
			case i < 0:
				return codegen.VM{}, fmt.Errorf("%w: no USB device %s is plugged into this machine", ErrInvalid, key)
			case host[i].InUse != "":
				return codegen.VM{}, fmt.Errorf("%w: this machine uses %s (%s)", ErrConflict, host[i].Name, host[i].InUse)
			}
			if o, ok := owners[key]; ok {
				return codegen.VM{}, fmt.Errorf("%w: %s is given to %s already", ErrConflict, host[i].Name, o.title)
			}
		}
	}

	// taken back first, so that a failure halfway leaves the VM with less, not more
	for _, u := range d.USB {
		if slices.Contains(want, u) {
			continue
		}

		devXML, err := libvirt.BuildUSBHostdevXML(u)
		if err != nil {
			return codegen.VM{}, err
		}

		flags := golibvirt.DomainDeviceModifyConfig
		if running && slices.Contains(live.USB, u) {
			flags |= golibvirt.DomainDeviceModifyLive
		}
		if err := l.DomainDetachDeviceFlags(dom, devXML, uint32(flags)); err != nil {
			return codegen.VM{}, fmt.Errorf("take back USB device %s:%s: %w", u.Vendor, u.Product, err)
		}

		logger.Info("took a USB device back", zap.String("vm", d.Name), zap.String("device", u.Vendor+":"+u.Product))
	}

	for _, u := range add {
		devXML, err := libvirt.BuildUSBHostdevXML(u)
		if err != nil {
			return codegen.VM{}, err
		}

		flags := golibvirt.DomainDeviceModifyConfig
		if running {
			flags |= golibvirt.DomainDeviceModifyLive
		}
		if err := l.DomainAttachDeviceFlags(dom, devXML, uint32(flags)); err != nil {
			if libvirt.IsOperationInvalid(err) {
				return codegen.VM{}, fmt.Errorf("%w: %s", ErrConflict, err)
			}
			return codegen.VM{}, fmt.Errorf("give USB device %s:%s: %w", u.Vendor, u.Product, err)
		}

		logger.Info("gave a USB device", zap.String("vm", d.Name), zap.String("device", u.Vendor+":"+u.Product), zap.Bool("live", running))
	}

	return s.toVM(l, dom)
}
