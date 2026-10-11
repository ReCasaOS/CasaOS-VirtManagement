package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Snapshots are taken with the VM off, as qcow2 internal snapshots made by
// qemu-img, plus a copy of the UEFI variables. libvirt refuses internal
// snapshots of a VM with pflash firmware, and its external snapshots cannot
// be deleted once the VM went back to one (libvirt 10 and 12).

const snapshotTimeout = 5 * time.Minute

// forSnapshots looks a VM up for a snapshot operation: one CasaOS created,
// shut off, with only qcow2 disks CasaOS created.
func (s *VMs) forSnapshots(id uuid.UUID) (*golibvirt.Libvirt, golibvirt.Domain, libvirt.Domain, error) {
	l, dom, err := s.lookup(id)
	if err != nil {
		return nil, dom, libvirt.Domain{}, err
	}

	state, err := s.state(l, dom)
	if err != nil {
		return nil, dom, libvirt.Domain{}, err
	}
	if state != libvirt.StateShutOff && state != libvirt.StateCrashed {
		return nil, dom, libvirt.Domain{}, fmt.Errorf("%w: snapshots are taken and restored with the VM off; shut it down first", ErrConflict)
	}

	desc, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
	if err != nil {
		return nil, dom, libvirt.Domain{}, err
	}

	d, err := libvirt.ParseDomainXML(desc)
	if err != nil {
		return nil, dom, libvirt.Domain{}, err
	}

	if d.Metadata == nil {
		return nil, dom, libvirt.Domain{}, fmt.Errorf("%w: CasaOS did not create this VM, so it does not take snapshots of it", ErrConflict)
	}

	for _, disk := range d.Disks {
		if !s.ownsDisk(d, disk.Path) {
			return nil, dom, libvirt.Domain{}, fmt.Errorf("%w: CasaOS did not create %s, so it does not take snapshots of it", ErrConflict, disk.Path)
		}
	}

	return l, dom, d, nil
}

// CreateSnapshot saves the disks and UEFI variables of a VM that is off.
func (s *VMs) CreateSnapshot(id uuid.UUID, name string) (codegen.VM, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return codegen.VM{}, fmt.Errorf("%w: the snapshot has no name", ErrInvalid)
	}

	l, dom, d, err := s.forSnapshots(id)
	if err != nil {
		return codegen.VM{}, err
	}

	ctx, cancel := context.WithTimeout(s.ctx, snapshotTimeout)
	defer cancel()

	snap := libvirt.Snapshot{ID: nextSnapshotID(d.Metadata.Snapshots), Name: name, Created: time.Now().UTC().Format(time.RFC3339)}

	var done []string
	undo := func() {
		for _, path := range done {
			if err := qemuImg(ctx, "snapshot", "-d", snap.ID, path); err != nil {
				logger.Error("error when undoing a snapshot", zap.String("path", path), zap.Error(err))
			}
		}
		_ = os.Remove(nvramCopy(d.NVRAM, snap.ID))
	}

	for _, disk := range d.Disks {
		if err := upgradeQcow2(ctx, disk.Path); err != nil {
			undo()
			return codegen.VM{}, err
		}
		if err := qemuImg(ctx, "snapshot", "-c", snap.ID, disk.Path); err != nil {
			undo()
			return codegen.VM{}, fmt.Errorf("snapshot %s: %w", disk.Path, err)
		}
		done = append(done, disk.Path)
	}

	// before the first start there are no variables yet: going back to
	// this snapshot then removes them, and libvirt makes them afresh
	if d.NVRAM != "" {
		if err := copyFile(d.NVRAM, nvramCopy(d.NVRAM, snap.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			undo()
			return codegen.VM{}, fmt.Errorf("save the UEFI variables: %w", err)
		}
	}

	m := *d.Metadata
	m.Snapshots = append(slices.Clone(m.Snapshots), snap)
	if err := setMetadata(l, dom, m, false); err != nil {
		undo()
		return codegen.VM{}, fmt.Errorf("record the snapshot: %w", err)
	}

	logger.Info("took a snapshot", zap.String("vm", d.Name), zap.String("snapshot", snap.ID), zap.String("name", name))

	return s.toVM(l, dom)
}

// RevertSnapshot puts the disks and UEFI variables of a VM that is off back
// as the snapshot saved them.
func (s *VMs) RevertSnapshot(id uuid.UUID, snapshotID string) (codegen.VM, error) {
	l, dom, d, err := s.forSnapshots(id)
	if err != nil {
		return codegen.VM{}, err
	}

	if !slices.ContainsFunc(d.Metadata.Snapshots, func(sn libvirt.Snapshot) bool { return sn.ID == snapshotID }) {
		return codegen.VM{}, fmt.Errorf("%w: the VM has no snapshot %s", ErrNotFound, snapshotID)
	}

	ctx, cancel := context.WithTimeout(s.ctx, snapshotTimeout)
	defer cancel()

	// every disk must have it before any is changed
	for _, disk := range d.Disks {
		ids, err := qcow2Snapshots(ctx, disk.Path)
		if err != nil {
			return codegen.VM{}, err
		}
		if !slices.Contains(ids, snapshotID) {
			return codegen.VM{}, fmt.Errorf("%w: %s has no snapshot %s", ErrConflict, disk.Path, snapshotID)
		}
	}

	for _, disk := range d.Disks {
		if err := qemuImg(ctx, "snapshot", "-a", snapshotID, disk.Path); err != nil {
			return codegen.VM{}, fmt.Errorf("go back to the snapshot on %s: %w", disk.Path, err)
		}
	}

	if d.NVRAM != "" {
		saved := nvramCopy(d.NVRAM, snapshotID)
		if _, err := os.Stat(saved); err == nil {
			if err := copyFile(saved, d.NVRAM); err != nil {
				return codegen.VM{}, fmt.Errorf("put back the UEFI variables: %w", err)
			}
		} else if err := os.Remove(d.NVRAM); err != nil && !errors.Is(err, os.ErrNotExist) {
			return codegen.VM{}, fmt.Errorf("reset the UEFI variables: %w", err)
		}
	}

	logger.Info("went back to a snapshot", zap.String("vm", d.Name), zap.String("snapshot", snapshotID))

	return s.toVM(l, dom)
}

// DeleteSnapshot deletes a snapshot of a VM that is off.
func (s *VMs) DeleteSnapshot(id uuid.UUID, snapshotID string) (codegen.VM, error) {
	l, dom, d, err := s.forSnapshots(id)
	if err != nil {
		return codegen.VM{}, err
	}

	if !slices.ContainsFunc(d.Metadata.Snapshots, func(sn libvirt.Snapshot) bool { return sn.ID == snapshotID }) {
		return codegen.VM{}, fmt.Errorf("%w: the VM has no snapshot %s", ErrNotFound, snapshotID)
	}

	ctx, cancel := context.WithTimeout(s.ctx, snapshotTimeout)
	defer cancel()

	for _, disk := range d.Disks {
		ids, err := qcow2Snapshots(ctx, disk.Path)
		if err != nil {
			return codegen.VM{}, err
		}
		if !slices.Contains(ids, snapshotID) {
			continue // gone already
		}
		if err := qemuImg(ctx, "snapshot", "-d", snapshotID, disk.Path); err != nil {
			return codegen.VM{}, fmt.Errorf("delete the snapshot on %s: %w", disk.Path, err)
		}
	}

	if d.NVRAM != "" {
		if err := os.Remove(nvramCopy(d.NVRAM, snapshotID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Error("error when deleting saved UEFI variables", zap.String("vm", d.Name), zap.Error(err))
		}
	}

	m := *d.Metadata
	m.Snapshots = slices.DeleteFunc(slices.Clone(m.Snapshots), func(sn libvirt.Snapshot) bool { return sn.ID == snapshotID })
	if err := setMetadata(l, dom, m, false); err != nil {
		return codegen.VM{}, fmt.Errorf("forget the snapshot: %w", err)
	}

	logger.Info("deleted a snapshot", zap.String("vm", d.Name), zap.String("snapshot", snapshotID))

	return s.toVM(l, dom)
}

// removeSnapshotFiles deletes what snapshots keep outside the disks, for a VM
// being deleted.
func removeSnapshotFiles(d libvirt.Domain) {
	if d.Metadata == nil || d.NVRAM == "" {
		return
	}

	for _, sn := range d.Metadata.Snapshots {
		if err := os.Remove(nvramCopy(d.NVRAM, sn.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Error("error when deleting saved UEFI variables", zap.String("vm", d.Name), zap.Error(err))
		}
	}
}

// nextSnapshotID is s<n>, one more than the highest in use.
func nextSnapshotID(snaps []libvirt.Snapshot) string {
	highest := 0
	for _, sn := range snaps {
		if n, err := strconv.Atoi(strings.TrimPrefix(sn.ID, "s")); err == nil && n > highest {
			highest = n
		}
	}

	return "s" + strconv.Itoa(highest+1)
}

// nvramCopy is where a snapshot keeps the UEFI variables: next to them, in
// libvirt's root-only folder.
func nvramCopy(nvram, snapshotID string) string {
	return nvram + ".snapshot-" + snapshotID
}

func qemuImg(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "qemu-img", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img %s: %s", args[0], commandError(err, out))
	}

	return nil
}

// qcow2Snapshots lists the internal snapshot names of a qcow2 file.
func qcow2Snapshots(ctx context.Context, path string) ([]string, error) {
	info, err := qcow2Info(ctx, path)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(info.Snapshots))
	for _, sn := range info.Snapshots {
		names = append(names, sn.Name)
	}

	return names, nil
}

type qcow2InfoJSON struct {
	Format    string `json:"format"`
	Snapshots []struct {
		Name string `json:"name"`
	} `json:"snapshots"`
	FormatSpecific struct {
		Data struct {
			Compat string `json:"compat"`
		} `json:"data"`
	} `json:"format-specific"`
}

func qcow2Info(ctx context.Context, path string) (qcow2InfoJSON, error) {
	var info qcow2InfoJSON

	out, err := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", "-f", "qcow2", path).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return info, fmt.Errorf("qemu-img info: %s", commandError(err, exitErr.Stderr))
		}
		return info, fmt.Errorf("qemu-img info: %w", err)
	}

	if err := json.Unmarshal(out, &info); err != nil {
		return info, fmt.Errorf("qemu-img info: %w", err)
	}

	return info, nil
}

// upgradeQcow2 turns a qcow2 0.10 file, which libvirt used to create, into
// 1.1: a 0.10 file with snapshots can no longer grow.
func upgradeQcow2(ctx context.Context, path string) error {
	info, err := qcow2Info(ctx, path)
	if err != nil {
		return err
	}

	if info.FormatSpecific.Data.Compat != "0.10" {
		return nil
	}

	if err := qemuImg(ctx, "amend", "-f", "qcow2", "-o", "compat=1.1", path); err != nil {
		return fmt.Errorf("upgrade %s to qcow2 1.1: %w", path, err)
	}

	logger.Info("upgraded a disk to qcow2 1.1", zap.String("path", path))
	return nil
}

// copyFile copies src over dst, keeping src's mode.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	st, err := in.Stat()
	if err != nil {
		return err
	}

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	return os.Rename(tmp, dst)
}
