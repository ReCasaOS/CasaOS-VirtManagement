package service

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// diskVM is a VM whose disks are about to change: its definition, what runs
// when it is running, and whether it is.
type diskVM struct {
	l       *golibvirt.Libvirt
	dom     golibvirt.Domain
	next    libvirt.Domain // the definition
	live    libvirt.Domain // what runs; the definition when the VM is off
	running bool           // running or paused: QEMU has the disks open
}

// forDisks looks a VM up for a disk change: one CasaOS created, not being
// created, and in a state where its disks can be touched.
func (s *VMs) forDisks(id uuid.UUID) (diskVM, error) {
	l, dom, err := s.lookup(id)
	if err != nil {
		return diskVM{}, err
	}

	state, err := s.state(l, dom)
	if err != nil {
		return diskVM{}, err
	}

	switch state {
	case libvirt.StateRunning, libvirt.StatePaused, libvirt.StateShutOff, libvirt.StateCrashed:
	case libvirt.StateCreating:
		return diskVM{}, fmt.Errorf("%w: the VM is still being created", ErrConflict)
	default:
		return diskVM{}, fmt.Errorf("%w: the VM is %s; wait until it is running or off", ErrConflict, state)
	}

	inactive, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
	if err != nil {
		return diskVM{}, err
	}

	next, err := libvirt.ParseDomainXML(inactive)
	if err != nil {
		return diskVM{}, err
	}

	if next.Metadata == nil {
		return diskVM{}, fmt.Errorf("%w: CasaOS did not create this VM, so it does not change its disks", ErrConflict)
	}

	vm := diskVM{l: l, dom: dom, next: next, live: next}
	vm.running = state == libvirt.StateRunning || state == libvirt.StatePaused

	if vm.running {
		desc, err := l.DomainGetXMLDesc(dom, 0)
		if err != nil {
			return diskVM{}, err
		}

		if vm.live, err = libvirt.ParseDomainXML(desc); err != nil {
			return diskVM{}, err
		}
	}

	return vm, nil
}

// setVolumes records the disk files CasaOS created for the VM, in the
// definition and, while it runs, in the live VM, which the API reads.
func (vm diskVM) setVolumes(volumes []string) error {
	m := *vm.next.Metadata
	m.Volumes = volumes

	return setMetadata(vm.l, vm.dom, m, vm.running)
}

// newDiskPath picks an unused file name for another disk of the VM:
// <name>-1.qcow2, <name>-2.qcow2, …, skipping files a removed disk left.
func newDiskPath(storagePath, name string) (string, error) {
	for n := 1; n < 1000; n++ {
		path := filepath.Join(storagePath, name+"-"+strconv.Itoa(n)+".qcow2")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path, nil
		} else if err != nil {
			return "", err
		}
	}

	return "", fmt.Errorf("could not find a free disk name for %s", name)
}

// AddDisk creates an empty disk and adds it to the VM, plugged in at once when
// the VM runs and can take it, or at its next start otherwise.
func (s *VMs) AddDisk(id uuid.UUID, sizeGiB int) (codegen.VM, error) {
	if sizeGiB < 1 {
		return codegen.VM{}, fmt.Errorf("%w: a disk is at least 1 GiB", ErrInvalid)
	}

	vm, err := s.forDisks(id)
	if err != nil {
		return codegen.VM{}, err
	}
	l := vm.l

	if len(vm.next.Metadata.Snapshots) > 0 {
		return codegen.VM{}, fmt.Errorf("%w: the VM has snapshots, which a new disk would not be in; delete them first", ErrConflict)
	}

	// a target the running VM still has, or the definition already gives
	// another disk, is taken
	targets := libvirt.Domain{Targets: append(slices.Clone(vm.next.Targets), vm.live.Targets...)}
	target, err := targets.NextDiskTarget()
	if err != nil {
		return codegen.VM{}, fmt.Errorf("%w: %s", ErrConflict, err)
	}

	// next to the disk the VM boots from, when CasaOS made it: a VM whose
	// disk is on another drive keeps its disks together
	storagePath := config.StoragePath()
	if len(vm.next.Disks) > 0 && s.ownsDisk(vm.next, vm.next.Disks[0].Path) {
		storagePath = filepath.Dir(vm.next.Disks[0].Path)
	}

	pool, err := libvirt.EnsurePool(l, storagePath)
	if err != nil {
		return codegen.VM{}, err
	}

	path, err := newDiskPath(storagePath, vm.next.Name)
	if err != nil {
		return codegen.VM{}, err
	}

	volXML, err := libvirt.BuildVolumeXML(filepath.Base(path), int64(sizeGiB)*gib)
	if err != nil {
		return codegen.VM{}, err
	}

	diskXML, err := libvirt.BuildDiskXML(path, target)
	if err != nil {
		return codegen.VM{}, err
	}

	vol, err := l.StorageVolCreateXML(pool, volXML, 0)
	if err != nil {
		return codegen.VM{}, fmt.Errorf("create disk: %w", err)
	}

	// recorded first: a disk CasaOS created must never be left attached and
	// not deletable with the VM
	volumes := append(slices.Clone(vm.next.Metadata.Volumes), path)
	if err := vm.setVolumes(volumes); err != nil {
		dropVolume(l, vol, path)
		return codegen.VM{}, fmt.Errorf("record disk: %w", err)
	}

	plugged := false
	if vm.running {
		// libvirt changes neither the VM nor its definition when this fails
		err := l.DomainAttachDeviceFlags(vm.dom, diskXML, uint32(golibvirt.DomainDeviceModifyLive|golibvirt.DomainDeviceModifyConfig))
		if err != nil {
			logger.Info("could not plug a disk into a running VM, adding it for the next start", zap.String("vm", vm.next.Name), zap.Error(err))
		}
		plugged = err == nil
	}

	if !plugged {
		err = l.DomainAttachDeviceFlags(vm.dom, diskXML, uint32(golibvirt.DomainDeviceModifyConfig))
	}
	if err != nil {
		if err := vm.setVolumes(vm.next.Metadata.Volumes); err != nil {
			logger.Error("error when forgetting a disk that could not be added", zap.String("vm", vm.next.Name), zap.Error(err))
		}
		dropVolume(l, vol, path)

		if libvirt.IsOperationInvalid(err) {
			return codegen.VM{}, fmt.Errorf("%w: %s", ErrConflict, err)
		}
		return codegen.VM{}, fmt.Errorf("add disk: %w", err)
	}

	logger.Info("added a disk", zap.String("vm", vm.next.Name), zap.String("target", target), zap.String("path", path))

	return s.toVM(l, vm.dom)
}

func dropVolume(l *golibvirt.Libvirt, vol golibvirt.StorageVol, path string) {
	if err := l.StorageVolDelete(vol, golibvirt.StorageVolDeleteNormal); err != nil {
		logger.Error("error when removing a disk that could not be added", zap.String("path", path), zap.Error(err))
	}
}

// ResizeDisk grows a disk CasaOS created: through QEMU when the running VM
// has it open, on the file otherwise.
func (s *VMs) ResizeDisk(id uuid.UUID, target string, sizeGiB int) (codegen.VM, error) {
	vm, err := s.forDisks(id)
	if err != nil {
		return codegen.VM{}, err
	}
	l := vm.l

	disk, ok := vm.next.DiskByTarget(target)
	if !ok {
		return codegen.VM{}, fmt.Errorf("%w: the VM has no disk %s", ErrDiskNotFound, target)
	}

	if !s.ownsDisk(vm.next, disk.Path) {
		return codegen.VM{}, fmt.Errorf("%w: CasaOS did not create %s, so it does not resize it", ErrConflict, disk.Path)
	}

	vol, err := lookupVolume(l, disk.Path)
	if err != nil {
		return codegen.VM{}, err
	}

	_, capacity, _, err := l.StorageVolGetInfo(vol)
	if err != nil {
		return codegen.VM{}, err
	}

	size := uint64(sizeGiB) * uint64(gib)
	if size <= capacity {
		return codegen.VM{}, fmt.Errorf("%w: the disk is %d MiB already; disks only grow", ErrInvalid, capacity>>20)
	}

	if live, ok := vm.live.DiskByTarget(target); vm.running && ok && live.Path == disk.Path {
		err = l.DomainBlockResize(vm.dom, target, size, golibvirt.DomainBlockResizeBytes)
	} else {
		err = l.StorageVolResize(vol, size, 0)
	}
	if err != nil {
		return codegen.VM{}, fmt.Errorf("resize disk: %w", err)
	}

	logger.Info("resized a disk", zap.String("vm", vm.next.Name), zap.String("target", target), zap.Uint64("bytes", size))

	return s.toVM(l, vm.dom)
}

// RemoveDisk takes a disk out of the VM, and deletes its file when deleteFile
// is set and CasaOS created it. The VM must not have the disk open.
func (s *VMs) RemoveDisk(id uuid.UUID, target string, deleteFile bool) (codegen.VM, error) {
	vm, err := s.forDisks(id)
	if err != nil {
		return codegen.VM{}, err
	}
	l := vm.l

	disk, ok := vm.next.DiskByTarget(target)
	if !ok {
		return codegen.VM{}, fmt.Errorf("%w: the VM has no disk %s", ErrDiskNotFound, target)
	}

	if vm.next.Disks[0] == disk {
		return codegen.VM{}, fmt.Errorf("%w: the VM boots from %s; delete the VM instead", ErrConflict, target)
	}

	if len(vm.next.Metadata.Snapshots) > 0 {
		return codegen.VM{}, fmt.Errorf("%w: the VM has snapshots, which need all its disks; delete them first", ErrConflict)
	}

	// unplugging a disk the guest uses depends on the guest letting go of it
	if live, ok := vm.live.DiskByTarget(target); vm.running && ok && live.Path == disk.Path {
		return codegen.VM{}, fmt.Errorf("%w: the VM is using %s; shut it down first", ErrConflict, target)
	}

	diskXML, err := libvirt.BuildDiskXML(disk.Path, target)
	if err != nil {
		return codegen.VM{}, err
	}

	if err := l.DomainDetachDeviceFlags(vm.dom, diskXML, uint32(golibvirt.DomainDeviceModifyConfig)); err != nil {
		return codegen.VM{}, fmt.Errorf("remove disk: %w", err)
	}

	owned := s.ownsDisk(vm.next, disk.Path)
	if owned {
		volumes := slices.DeleteFunc(slices.Clone(vm.next.Metadata.Volumes), func(v string) bool { return v == disk.Path })
		if err := vm.setVolumes(volumes); err != nil {
			logger.Error("error when forgetting a removed disk", zap.String("vm", vm.next.Name), zap.String("path", disk.Path), zap.Error(err))
		}
	}

	logger.Info("removed a disk", zap.String("vm", vm.next.Name), zap.String("target", target), zap.String("path", disk.Path))

	if deleteFile && owned {
		if err := deleteVolume(l, disk.Path); err != nil {
			return codegen.VM{}, fmt.Errorf("the disk was removed from the VM, but %s could not be deleted: %w", disk.Path, err)
		}
	}

	return s.toVM(l, vm.dom)
}
