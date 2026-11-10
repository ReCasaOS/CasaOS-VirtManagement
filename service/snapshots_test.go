package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
)

func TestNextSnapshotID(t *testing.T) {
	cases := []struct {
		have []libvirt.Snapshot
		want string
	}{
		{nil, "s1"},
		{[]libvirt.Snapshot{{ID: "s1"}, {ID: "s3"}}, "s4"},
		{[]libvirt.Snapshot{{ID: "s2"}, {ID: "odd"}}, "s3"},
	}

	for _, c := range cases {
		if got := nextSnapshotID(c.have); got != c.want {
			t.Errorf("nextSnapshotID(%v) = %s, want %s", c.have, got, c.want)
		}
	}
}

func TestSnapshotStateOK(t *testing.T) {
	cases := []struct {
		state      string
		take, back bool
	}{
		{libvirt.StateShutOff, true, true},
		{libvirt.StateCrashed, true, true},
		{libvirt.StateRunning, true, false},
		{libvirt.StatePaused, true, false},
		{libvirt.StateCreating, false, false},
		{"pmsuspended", false, false},
	}

	for _, c := range cases {
		if got := snapshotStateOK(c.state, true); got != c.take {
			t.Errorf("take a snapshot when %s = %v, want %v", c.state, got, c.take)
		}
		if got := snapshotStateOK(c.state, false); got != c.back {
			t.Errorf("go back or delete when %s = %v, want %v", c.state, got, c.back)
		}
	}
}

// what QEMU 8 answers for a libvirt VM: CD-ROM, boot disk, data disk
const queryBlock = `[
	{"device": "", "qdev": "/machine/peripheral/sata0-0-0", "removable": true, "inserted": {"file": "/DATA/VM/iso/debian.iso", "node-name": "libvirt-3-format", "drv": "raw"}},
	{"device": "", "qdev": "/machine/peripheral/virtio-disk0/virtio-backend", "inserted": {"file": "/DATA/VM/web/disk.qcow2", "node-name": "libvirt-2-format", "drv": "qcow2"}},
	{"device": "", "qdev": "/machine/peripheral/virtio-disk1/virtio-backend", "inserted": {"file": "json:{\"driver\": \"qcow2\", \"file\": {\"driver\": \"file\", \"filename\": \"/DATA/VM/web/data.qcow2\"}}", "node-name": "libvirt-1-format", "drv": "qcow2"}},
	{"device": "", "qdev": "/machine/peripheral/sata0-0-1", "removable": true}
]`

func TestDiskNodes(t *testing.T) {
	disks := []libvirt.Disk{{Path: "/DATA/VM/web/disk.qcow2"}, {Path: "/DATA/VM/web/data.qcow2"}}

	nodes, err := diskNodes(json.RawMessage(queryBlock), disks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(nodes, ",") != "libvirt-2-format,libvirt-1-format" {
		t.Errorf("nodes = %v", nodes)
	}

	// a disk added to the definition only, until the next start
	_, err = diskNodes(json.RawMessage(queryBlock), append(disks, libvirt.Disk{Path: "/DATA/VM/web/new.qcow2"}))
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "new.qcow2") {
		t.Errorf("missing disk: err = %v, want a conflict naming it", err)
	}

	_, err = diskNodes(json.RawMessage(queryBlock), []libvirt.Disk{{Path: "/DATA/VM/iso/debian.iso"}})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("raw disk: err = %v, want a conflict", err)
	}
}

func TestSnapshotTransaction(t *testing.T) {
	body, err := json.Marshal(snapshotTransaction([]string{"libvirt-2-format", "libvirt-1-format"}, "s3"))
	if err != nil {
		t.Fatal(err)
	}

	want := `{"arguments":{"actions":[` +
		`{"data":{"device":"libvirt-2-format","name":"s3"},"type":"blockdev-snapshot-internal-sync"},` +
		`{"data":{"device":"libvirt-1-format","name":"s3"},"type":"blockdev-snapshot-internal-sync"}]},` +
		`"execute":"transaction"}`
	if string(body) != want {
		t.Errorf("transaction =\n%s\nwant\n%s", body, want)
	}
}

func TestQMPReturn(t *testing.T) {
	ret, err := qmpReturn("transaction", `{"return": {}, "id": "libvirt-42"}`)
	if err != nil || string(ret) != "{}" {
		t.Errorf("success: %s, %v", ret, err)
	}

	// libvirt passes QMP errors back as the answer
	_, err = qmpReturn("transaction", `{"id": "libvirt-43", "error": {"class": "GenericError", "desc": "Snapshot with name 's3' already exists"}}`)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("QMP error: err = %v", err)
	}
}
