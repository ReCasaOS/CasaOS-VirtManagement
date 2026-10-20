package libvirt

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if got != string(want) {
		t.Errorf("%s differs from the golden file; run go test -update to rewrite it if the change is intended\n%s", name, got)
	}
}

func TestBuildDomainXML(t *testing.T) {
	cases := map[string]DomainSpec{
		"domain_uefi_iso.xml": {
			Name: "1a2b3c4d", Title: `Alpine <test> & "more"`, VCPUs: 2, MemoryMiB: 1024,
			Firmware: FirmwareUEFI, DiskPath: "/DATA/VM/1a2b3c4d.qcow2", ISOPath: "/DATA/VM/iso/alpine.iso",
			NIC: NIC{Type: NICNetwork, Source: "default"}, Arch: "x86_64",
		},
		"domain_bios_disk.xml": {
			Name: "deadbeef", Title: "Home Assistant", VCPUs: 1, MemoryMiB: 512,
			Firmware: FirmwareBIOS, DiskPath: "/DATA/VM/deadbeef.qcow2",
			NIC: NIC{Type: NICNetwork, Source: "default"}, Arch: "x86_64",
		},
		"domain_uefi_arm64.xml": {
			Name: "0badcafe", Title: "arm", VCPUs: 4, MemoryMiB: 4096,
			Firmware: FirmwareUEFI, DiskPath: "/DATA/VM/0badcafe.qcow2",
			NIC: NIC{Type: NICNetwork, Source: "default"}, Arch: "aarch64",
		},
	}

	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := BuildDomainXML(spec)
			if err != nil {
				t.Fatal(err)
			}
			golden(t, name, got)

			d, err := ParseDomainXML(got)
			if err != nil {
				t.Fatal(err)
			}

			if d.Name != spec.Name || d.Title != spec.Title || d.VCPUs != spec.VCPUs || d.MemoryMiB != spec.MemoryMiB {
				t.Errorf("round trip: got %+v", d)
			}
			if d.UEFI != (spec.Firmware == FirmwareUEFI) {
				t.Errorf("round trip: UEFI = %v", d.UEFI)
			}
			if len(d.Disks) != 1 || d.Disks[0] != (Disk{Path: spec.DiskPath, Target: BootDiskTarget}) || !d.HasCDROM || d.CDROM != spec.ISOPath || !d.VNC {
				t.Errorf("round trip: devices %+v", d)
			}
			if d.Metadata == nil || len(d.Metadata.Volumes) != 1 || d.Metadata.Volumes[0] != spec.DiskPath || d.Metadata.Firmware != spec.Firmware {
				t.Errorf("round trip: metadata %+v", d.Metadata)
			}
		})
	}
}

func TestBuildDomainXMLRefusesBIOSOnArm(t *testing.T) {
	if _, err := BuildDomainXML(DomainSpec{Firmware: FirmwareBIOS, Arch: "aarch64"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestBuildCDROMXML(t *testing.T) {
	got, err := BuildCDROMXML("/DATA/VM/iso/a'b.iso", 1)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "cdrom_insert.xml", got)

	got, err = BuildCDROMXML("", 0)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "cdrom_eject.xml", got)
}

// A live domain as libvirt 12 returns it (trimmed), with the metadata written
// back under libvirt's own prefix and the VNC socket filled in.
const liveDomainXML = `<domain type='kvm' id='3'>
  <name>1a2b3c4d</name>
  <uuid>bc7e04a8-f296-4d64-bba0-48ce9c0849b6</uuid>
  <title>Alpine</title>
  <metadata>
    <casaos:vm xmlns:casaos="https://recasaos.github.io/xmlns/vm/1">
      <casaos:volume>/DATA/VM/1a2b3c4d.qcow2</casaos:volume>
      <casaos:firmware>uefi</casaos:firmware>
    </casaos:vm>
  </metadata>
  <memory unit='KiB'>1048576</memory>
  <vcpu placement='static'>2</vcpu>
  <os>
    <type arch='x86_64' machine='pc-q35-10.2'>hvm</type>
    <loader readonly='yes' type='pflash' format='raw'>/usr/share/OVMF/OVMF_CODE_4M.fd</loader>
  </os>
  <devices>
    <disk type='file' device='disk'><source file='/DATA/VM/1a2b3c4d.qcow2'/><target dev='vda' bus='virtio'/></disk>
    <disk type='file' device='cdrom'><target dev='sda' bus='sata'/><readonly/></disk>
    <graphics type='vnc' socket='/var/lib/libvirt/qemu/domain-3-1a2b3c4d/vnc.sock'>
      <listen type='socket' socket='/var/lib/libvirt/qemu/domain-3-1a2b3c4d/vnc.sock'/>
    </graphics>
  </devices>
</domain>`

func TestBuildDiskXML(t *testing.T) {
	got, err := BuildDiskXML("/DATA/VM/1a2b3c4d-1.qcow2", "vdb")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "disk_attach.xml", got)
}

func TestBuildMetadataXML(t *testing.T) {
	m := Metadata{Volumes: []string{"/DATA/VM/1a2b3c4d.qcow2", "/DATA/VM/1a2b3c4d-1.qcow2"}, Firmware: FirmwareUEFI}

	got, err := BuildMetadataXML(m)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "metadata.xml", got)

	// libvirt stores it in the namespace it is given, with that prefix
	stored := strings.NewReplacer("<vm>", `<casaos:vm xmlns:casaos="`+MetadataNamespace+`">`, "</vm>", "</casaos:vm>",
		"<volume>", "<casaos:volume>", "</volume>", "</casaos:volume>",
		"<firmware>", "<casaos:firmware>", "</firmware>", "</casaos:firmware>").Replace(got)
	d, err := ParseDomainXML("<domain><metadata>" + stored + "</metadata></domain>")
	if err != nil {
		t.Fatal(err)
	}
	if d.Metadata == nil || !slices.Equal(d.Metadata.Volumes, m.Volumes) || d.Metadata.Firmware != m.Firmware {
		t.Errorf("round trip: metadata %+v", d.Metadata)
	}
}

func TestParseLiveDomainXML(t *testing.T) {
	d, err := ParseDomainXML(liveDomainXML)
	if err != nil {
		t.Fatal(err)
	}

	if d.UUID != "bc7e04a8-f296-4d64-bba0-48ce9c0849b6" || d.MemoryMiB != 1024 || d.VCPUs != 2 || !d.UEFI {
		t.Errorf("got %+v", d)
	}
	if !d.HasCDROM || d.CDROM != "" {
		t.Errorf("empty CD-ROM: got HasCDROM=%v CDROM=%q", d.HasCDROM, d.CDROM)
	}
	if d.VNCSocket != "/var/lib/libvirt/qemu/domain-3-1a2b3c4d/vnc.sock" {
		t.Errorf("VNC socket %q", d.VNCSocket)
	}
	if d.Metadata == nil || d.Metadata.Firmware != "uefi" {
		t.Errorf("metadata %+v", d.Metadata)
	}
	if disk, ok := d.DiskByTarget("vda"); !ok || disk.Path != "/DATA/VM/1a2b3c4d.qcow2" {
		t.Errorf("DiskByTarget(vda) = %+v, %v", disk, ok)
	}
	if _, ok := d.DiskByTarget("sda"); ok {
		t.Error("DiskByTarget(sda) found the empty CD-ROM drive")
	}
}

func TestNextDiskTarget(t *testing.T) {
	cases := []struct {
		targets []string
		want    string
	}{
		{[]string{"vda", "sda"}, "vdb"},
		{[]string{"vda", "vdb", "vdd", "sda"}, "vdc"},
		{[]string{"hda"}, "vdb"}, // vda is left to the boot disk of a CasaOS VM
	}

	for _, tc := range cases {
		got, err := Domain{Targets: tc.targets}.NextDiskTarget()
		if err != nil || got != tc.want {
			t.Errorf("%v: got %q, %v, want %q", tc.targets, got, err, tc.want)
		}
	}

	var all []string
	for c := 'a'; c <= 'z'; c++ {
		all = append(all, "vd"+string(c))
	}
	if _, err := (Domain{Targets: all}).NextDiskTarget(); err == nil {
		t.Error("expected an error with every target taken")
	}
}

func TestParseForeignDomainXML(t *testing.T) {
	d, err := ParseDomainXML(`<domain type='kvm'><name>win</name><uuid>x</uuid><memory>2097152</memory><vcpu>2</vcpu><os><type>hvm</type></os>
		<devices><disk type='block' device='disk'><source dev='/dev/sdb'/></disk></devices></domain>`)
	if err != nil {
		t.Fatal(err)
	}

	if d.Metadata != nil || d.UEFI || d.VNC || len(d.Disks) != 0 || d.MemoryMiB != 2048 {
		t.Errorf("got %+v", d)
	}
}

func TestParseDomainDescription(t *testing.T) {
	d, err := ParseDomainXML(`<domain type='kvm'><name>web</name><title>Web</title><description>the shop</description></domain>`)
	if err != nil || d.Title != "Web" || d.Description != "the shop" {
		t.Errorf("%+v %v", d, err)
	}
}

func TestParseVCPUCurrent(t *testing.T) {
	d, err := ParseDomainXML(`<domain><name>a</name><vcpu current='2'>4</vcpu></domain>`)
	if err != nil {
		t.Fatal(err)
	}
	if d.VCPUs != 2 {
		t.Errorf("VCPUs = %d, want the current count 2", d.VCPUs)
	}
}

func TestBuildPoolAndVolumeXML(t *testing.T) {
	pool, err := BuildPoolXML(PoolName, "/DATA/VM")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "pool.xml", pool)

	vol, err := BuildVolumeXML("1a2b3c4d.qcow2", 32<<30)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "volume.xml", vol)
}

func TestCDROMBootOrder(t *testing.T) {
	d, err := ParseDomainXML(`<domain><devices><disk type='file' device='cdrom'><source file='/DATA/VM/iso/a.iso'/><target dev='sda' bus='sata'/><boot order='1'/></disk></devices></domain>`)
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasCDROM || d.CDROM != "/DATA/VM/iso/a.iso" || d.CDROMBoot != 1 {
		t.Errorf("CD-ROM %v %q boot %d", d.HasCDROM, d.CDROM, d.CDROMBoot)
	}
}

func TestUSBHostdev(t *testing.T) {
	got, err := BuildUSBHostdevXML(USBDevice{Vendor: "174f", Product: "244c"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "hostdev_usb.xml", got)

	d, err := ParseDomainXML("<domain><devices>" + got + `<hostdev mode='subsystem' type='pci'><source><address domain='0' bus='1' slot='0' function='0'/></source></hostdev></devices></domain>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.USB) != 1 || d.USB[0] != (USBDevice{Vendor: "174f", Product: "244c"}) {
		t.Errorf("USB %+v", d.USB)
	}

	if _, err := BuildUSBHostdevXML(USBDevice{Vendor: "zz", Product: "1"}); err == nil {
		t.Error("a bad id should fail")
	}
}

func TestPoolNameFor(t *testing.T) {
	a, b := PoolNameFor("/media/big/VM"), PoolNameFor("/DATA/other")
	if a == b || a != PoolNameFor("/media/big/VM") || len(a) != len(PoolName)+9 {
		t.Errorf("names %q %q", a, b)
	}
}
