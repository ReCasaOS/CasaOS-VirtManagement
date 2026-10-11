package service

import (
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
