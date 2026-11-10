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

// Snapshots are qcow2 internal snapshots, without the memory, plus a copy of
// the UEFI variables. libvirt refuses internal snapshots of a VM with pflash
// firmware, and its external snapshots cannot be deleted once the VM went
// back to one (libvirt 10 and 12). So with the VM off qemu-img takes them,
// and with the VM on its own QEMU does, over QMP: the same kind of snapshot,
// which qemu-img later goes back to or deletes with the VM off.

const snapshotTimeout = 5 * time.Minute

// snapshotStateOK tells whether a VM in state can take a snapshot (take) or
// go back to or delete one: only taking one works with the VM on.
func snapshotStateOK(state string, take bool) bool {
	switch state {
	case libvirt.StateShutOff, libvirt.StateCrashed:
		return true
	case libvirt.StateRunning, libvirt.StatePaused:
		return take
	}

	return false
}

// forSnapshots looks a VM up for a snapshot operation: one CasaOS created,
// in a state snapshotStateOK accepts, with only qcow2 disks CasaOS created.
func (s *VMs) forSnapshots(id uuid.UUID, take bool) (*golibvirt.Libvirt, golibvirt.Domain, libvirt.Domain, string, error) {
	l, dom, err := s.lookup(id)
	if err != nil {
		return nil, dom, libvirt.Domain{}, "", err
	}

	state, err := s.state(l, dom)
	if err != nil {
		return nil, dom, libvirt.Domain{}, "", err
	}
	if !snapshotStateOK(state, take) {
		if take {
			return nil, dom, libvirt.Domain{}, "", fmt.Errorf("%w: the VM is %s; wait until it is on or off", ErrConflict, state)
		}
		return nil, dom, libvirt.Domain{}, "", fmt.Errorf("%w: snapshots are restored and deleted with the VM off; shut it down first", ErrConflict)
	}

	desc, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
	if err != nil {
		return nil, dom, libvirt.Domain{}, "", err
	}

	d, err := libvirt.ParseDomainXML(desc)
	if err != nil {
		return nil, dom, libvirt.Domain{}, "", err
	}

	if d.Metadata == nil {
		return nil, dom, libvirt.Domain{}, "", fmt.Errorf("%w: CasaOS did not create this VM, so it does not take snapshots of it", ErrConflict)
	}

	for _, disk := range d.Disks {
		if !s.ownsDisk(d, disk.Path) {
			return nil, dom, libvirt.Domain{}, "", fmt.Errorf("%w: CasaOS did not create %s, so it does not take snapshots of it", ErrConflict, disk.Path)
		}
	}

	return l, dom, d, state, nil
}

// CreateSnapshot saves the disks and UEFI variables of a VM, off or on.
func (s *VMs) CreateSnapshot(id uuid.UUID, name string) (codegen.VM, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return codegen.VM{}, fmt.Errorf("%w: the snapshot has no name", ErrInvalid)
	}

	l, dom, d, state, err := s.forSnapshots(id, true)
	if err != nil {
		return codegen.VM{}, err
	}

	ctx, cancel := context.WithTimeout(s.ctx, snapshotTimeout)
	defer cancel()

	snap := libvirt.Snapshot{ID: nextSnapshotID(d.Metadata.Snapshots), Name: name, Created: time.Now().UTC().Format(time.RFC3339)}

	live := state == libvirt.StateRunning || state == libvirt.StatePaused

	var undo func()
	if live {
		undo, err = snapshotLive(ctx, l, dom, d, snap.ID)
	} else {
		undo, err = snapshotOff(ctx, d, snap.ID)
	}
	if err != nil {
		return codegen.VM{}, err
	}

	// the definition only: the API reads the list there, and writing it works
	// even if the VM stopped meanwhile
	m := *d.Metadata
	m.Snapshots = append(slices.Clone(m.Snapshots), snap)
	if err := setMetadata(l, dom, m, false); err != nil {
		undo()
		return codegen.VM{}, fmt.Errorf("record the snapshot: %w", err)
	}

	logger.Info("took a snapshot", zap.String("vm", d.Name), zap.String("snapshot", snap.ID), zap.String("name", name), zap.Bool("live", live))

	return s.toVM(l, dom)
}

// snapshotOff has qemu-img take snapshot id of every disk of a VM that is off.
// undo deletes what it made.
func snapshotOff(ctx context.Context, d libvirt.Domain, id string) (undo func(), err error) {
	var done []string
	undo = func() {
		for _, path := range done {
			if err := qemuImg(ctx, "snapshot", "-d", id, path); err != nil {
				logger.Error("error when undoing a snapshot", zap.String("path", path), zap.Error(err))
			}
		}
		_ = os.Remove(nvramCopy(d.NVRAM, id))
	}

	for _, disk := range d.Disks {
		if err := upgradeQcow2(ctx, disk.Path); err != nil {
			undo()
			return nil, err
		}
		if err := qemuImg(ctx, "snapshot", "-c", id, disk.Path); err != nil {
			undo()
			return nil, fmt.Errorf("snapshot %s: %w", disk.Path, err)
		}
		done = append(done, disk.Path)
	}

	if err := saveNVRAM(d, id); err != nil {
		undo()
		return nil, err
	}

	return undo, nil
}

// snapshotLive has the QEMU of a running or paused VM take snapshot id of
// every disk at once, in one QMP transaction, and saves the UEFI variables
// in the same instant. The guest file systems are frozen first when its
// agent answers. undo deletes what it made.
func snapshotLive(ctx context.Context, l *golibvirt.Libvirt, dom golibvirt.Domain, d libvirt.Domain, id string) (undo func(), err error) {
	for _, disk := range d.Disks {
		info, err := qcow2Info(ctx, disk.Path)
		if err != nil {
			return nil, err
		}
		// amend cannot upgrade a file QEMU holds
		if info.FormatSpecific.Data.Compat == "0.10" {
			return nil, fmt.Errorf("%w: %s is an old qcow2 0.10 file; take this first snapshot with the VM off, CasaOS upgrades it then", ErrConflict, disk.Path)
		}
	}

	blocks, err := qmp(l, dom, map[string]any{"execute": "query-block"})
	if err != nil {
		return nil, err
	}
	nodes, err := diskNodes(blocks, d.Disks)
	if err != nil {
		return nil, err
	}

	// read again: the user may have paused it since; a paused guest cannot
	// answer its agent, and stays paused
	state, _, err := l.DomainGetState(dom, 0)
	if err != nil {
		return nil, err
	}
	if golibvirt.DomainState(state) == golibvirt.DomainRunning {
		_, freezeErr := l.DomainFsfreeze(dom, nil, 0)
		if freezeErr != nil {
			logger.Info("snapshot without freezing the guest file systems", zap.String("vm", d.Name), zap.Error(freezeErr))
		}
		// thaw even after a failed freeze: the agent may have frozen late
		defer func() {
			if _, err := l.DomainFsthaw(dom, nil, 0); err != nil && freezeErr == nil {
				logger.Error("error when thawing the guest file systems; virsh domfsthaw does it", zap.String("vm", d.Name), zap.Error(err))
			}
		}()

		// stopped vCPUs cannot write the UEFI variables while they are copied
		if err := l.DomainSuspend(dom); err != nil {
			return nil, fmt.Errorf("pause the VM: %w", err)
		}
		defer func() {
			if err := l.DomainResume(dom); err != nil {
				logger.Error("error when resuming the VM after a snapshot", zap.String("vm", d.Name), zap.Error(err))
			}
		}()
	}

	if _, err := qmp(l, dom, snapshotTransaction(nodes, id)); err != nil {
		return nil, fmt.Errorf("snapshot the disks: %w", err)
	}

	undo = func() {
		for i, node := range nodes {
			cmd := map[string]any{"execute": "blockdev-snapshot-delete-internal-sync", "arguments": map[string]string{"device": node, "name": id}}
			if _, err := qmp(l, dom, cmd); err == nil {
				continue
			}
			// the VM stopped meanwhile: qemu-img can reach the file now
			if err := qemuImg(ctx, "snapshot", "-d", id, d.Disks[i].Path); err != nil {
				logger.Error("error when undoing a snapshot", zap.String("path", d.Disks[i].Path), zap.Error(err))
			}
		}
		_ = os.Remove(nvramCopy(d.NVRAM, id))
	}

	if err := saveNVRAM(d, id); err != nil {
		undo()
		return nil, err
	}

	return undo, nil
}

// saveNVRAM copies the UEFI variables for snapshot id. Before the first start
// there are none yet: going back to this snapshot then removes them, and
// libvirt makes them afresh.
func saveNVRAM(d libvirt.Domain, id string) error {
	if d.NVRAM == "" {
		return nil
	}
	if err := copyFile(d.NVRAM, nvramCopy(d.NVRAM, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("save the UEFI variables: %w", err)
	}

	return nil
}

// snapshotTransaction is the QMP command that snapshots every node at once:
// QEMU applies all or none.
func snapshotTransaction(nodes []string, id string) map[string]any {
	actions := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		actions = append(actions, map[string]any{
			"type": "blockdev-snapshot-internal-sync",
			"data": map[string]string{"device": node, "name": id},
		})
	}

	return map[string]any{"execute": "transaction", "arguments": map[string]any{"actions": actions}}
}

// diskNodes finds, in the answer to query-block, the node of each disk:
// the root node whose file is the disk, which must be qcow2.
func diskNodes(queryBlock json.RawMessage, disks []libvirt.Disk) ([]string, error) {
	type inserted struct {
		File     string `json:"file"`
		NodeName string `json:"node-name"`
		Drv      string `json:"drv"`
	}
	var blocks []struct {
		Inserted *inserted `json:"inserted"`
	}
	if err := json.Unmarshal(queryBlock, &blocks); err != nil {
		return nil, fmt.Errorf("query-block: %w", err)
	}

	nodes := make([]string, 0, len(disks))
	for _, disk := range disks {
		var found *inserted
		for _, b := range blocks {
			if b.Inserted != nil && qmpFilename(b.Inserted.File) == disk.Path {
				found = b.Inserted
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("%w: %s is not in the running VM yet; restart it first", ErrConflict, disk.Path)
		}
		if found.Drv != "qcow2" || found.NodeName == "" {
			return nil, fmt.Errorf("%w: QEMU runs %s as %q, not qcow2", ErrConflict, disk.Path, found.Drv)
		}
		nodes = append(nodes, found.NodeName)
	}

	return nodes, nil
}

// qmpFilename is the path of a QEMU node's file, which QEMU writes as
// json:{...} when options do not fit a plain name.
func qmpFilename(file string) string {
	spec, ok := strings.CutPrefix(file, "json:")
	if !ok {
		return file
	}

	var opts struct {
		File struct {
			Filename string `json:"filename"`
		} `json:"file"`
	}
	if json.Unmarshal([]byte(spec), &opts) != nil {
		return file
	}

	return opts.File.Filename
}

// qmp runs a QMP command in the VM's QEMU and returns its "return".
// libvirt hands QMP errors back as an answer, not as an error.
func qmp(l *golibvirt.Libvirt, dom golibvirt.Domain, cmd map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(cmd)
	if err != nil {
		return nil, err
	}

	out, err := l.QEMUDomainMonitorCommand(dom, string(body), 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cmd["execute"], err)
	}

	return qmpReturn(cmd["execute"], out)
}

func qmpReturn(execute any, out string) (json.RawMessage, error) {
	var reply struct {
		Return json.RawMessage `json:"return"`
		Error  *struct {
			Class string `json:"class"`
			Desc  string `json:"desc"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		return nil, fmt.Errorf("%s: %w", execute, err)
	}
	if reply.Error != nil {
		return nil, fmt.Errorf("%s: %s", execute, reply.Error.Desc)
	}

	return reply.Return, nil
}

// RevertSnapshot puts the disks and UEFI variables of a VM that is off back
// as the snapshot saved them.
func (s *VMs) RevertSnapshot(id uuid.UUID, snapshotID string) (codegen.VM, error) {
	l, dom, d, _, err := s.forSnapshots(id, false)
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
	l, dom, d, _, err := s.forSnapshots(id, false)
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

	// -U: a running VM's QEMU holds the write lock
	out, err := exec.CommandContext(ctx, "qemu-img", "info", "-U", "--output=json", "-f", "qcow2", path).Output()
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
