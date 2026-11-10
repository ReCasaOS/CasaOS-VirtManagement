package libvirt

import (
	"errors"
	"strings"
	"testing"
)

func TestBuildShareXML(t *testing.T) {
	got, err := BuildShareXML(Share{Source: "/media/sda1/data_cam", Tag: "data_cam"}, "/usr/libexec/virtiofsd")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "filesystem_virtiofs.xml", got)

	// a folder name is data, not XML
	got, err = BuildShareXML(Share{Source: `/DATA/a"b<c>&d`, Tag: "t"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, `a"b<c>`) || strings.Contains(got, "<binary") {
		t.Fatalf("escaped, and no binary when none is given: %s", got)
	}

	for _, tag := range []string{"", "a b", "a/b", "x'y", strings.Repeat("a", 37), "città"} {
		if _, err := BuildShareXML(Share{Source: "/DATA/x", Tag: tag}, ""); err == nil {
			t.Errorf("tag %q accepted", tag)
		}
	}
}

const domainForMemory = `<domain type='kvm'>
  <name>1a2b3c4d</name>
  <metadata>
    <casaos:vm xmlns:casaos="https://recasaos.github.io/xmlns/vm/1">
      <casaos:volume>/DATA/VM/1a2b3c4d.qcow2</casaos:volume>
    </casaos:vm>
  </metadata>
  <memory unit='KiB'>2097152</memory>
  <currentMemory unit='KiB'>2097152</currentMemory>
  <vcpu placement='static'>2</vcpu>
  <devices>
    <memballoon model='virtio'><stats period='5'/></memballoon>
  </devices>
</domain>`

func TestWithSharedMemory(t *testing.T) {
	got, changed, err := WithSharedMemory(domainForMemory)
	if err != nil || !changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}

	// spliced after <currentMemory>, everything else byte for byte
	want := strings.Replace(domainForMemory, "</currentMemory>", "</currentMemory>"+sharedMemoryXML, 1)
	if got != want {
		t.Fatalf("got\n%s", got)
	}

	d, err := ParseDomainXML(got)
	if err != nil || !d.SharedMemory || d.Metadata == nil {
		t.Fatalf("parsed back: shared %v, metadata %v, err %v", d.SharedMemory, d.Metadata, err)
	}

	// already there: nothing to do
	again, changed, err := WithSharedMemory(got)
	if err != nil || changed || again != got {
		t.Fatalf("second pass: changed %v, err %v", changed, err)
	}

	// without <currentMemory>, after <memory>
	noCurrent := strings.Replace(domainForMemory, "  <currentMemory unit='KiB'>2097152</currentMemory>\n", "", 1)
	got, _, err = WithSharedMemory(noCurrent)
	if err != nil || !strings.Contains(got, "</memory>"+sharedMemoryXML) {
		t.Fatalf("after <memory>: %v\n%s", err, got)
	}

	// someone's own memory backing is not rewritten
	huge := strings.Replace(domainForMemory, "<vcpu", "<memoryBacking><hugepages/></memoryBacking>\n  <vcpu", 1)
	if _, _, err := WithSharedMemory(huge); !errors.Is(err, ErrMemoryBacking) {
		t.Fatalf("hugepages: %v", err)
	}
}

func TestParseShares(t *testing.T) {
	xml := strings.Replace(domainForMemory, "<memballoon", `<filesystem type='mount' accessmode='passthrough'>
      <driver type='virtiofs'/>
      <binary path='/usr/libexec/virtiofsd'/>
      <source dir='/media/sda1/data_cam'/>
      <target dir='data_cam'/>
    </filesystem>
    <filesystem type='mount' accessmode='mapped'>
      <source dir='/DATA/old9p'/>
      <target dir='old9p'/>
    </filesystem>
    <memballoon`, 1)

	d, err := ParseDomainXML(xml)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Shares) != 1 || d.Shares[0] != (Share{Source: "/media/sda1/data_cam", Tag: "data_cam"}) {
		t.Fatalf("only the virtiofs one is a share: %+v", d.Shares)
	}
	if d.SharedMemory {
		t.Fatal("no memory backing, no shared memory")
	}
}

func TestWithShares(t *testing.T) {
	cam := Share{Source: "/media/sda1/data_cam", Tag: "data_cam"}
	res := Share{Source: "/DATA/AppData/nifi/resources", Tag: "resources"}

	// two shares from nothing: both added, shared memory on, the rest untouched
	got, changed, err := WithShares(domainForMemory, []Share{cam, res}, "/usr/libexec/virtiofsd")
	if err != nil || !changed {
		t.Fatalf("changed %v err %v", changed, err)
	}
	d, err := ParseDomainXML(got)
	if err != nil || !d.SharedMemory || len(d.Shares) != 2 || d.Shares[0] != cam || d.Shares[1] != res || d.Metadata == nil {
		t.Fatalf("%+v %v\n%s", d.Shares, err, got)
	}
	if !strings.Contains(got, `<casaos:vm xmlns:casaos="https://recasaos.github.io/xmlns/vm/1">`) || !strings.Contains(got, "<memballoon model='virtio'><stats period='5'/></memballoon>") {
		t.Fatalf("the rest was rewritten:\n%s", got)
	}

	// the same again: nothing to do
	if same, changed, err := WithShares(got, []Share{cam, res}, "/usr/libexec/virtiofsd"); err != nil || changed || same != got {
		t.Fatalf("again: changed %v err %v", changed, err)
	}

	// libvirt writes a kept share back with its own additions: they stay
	withAlias := strings.Replace(got, `<target dir="data_cam"></target>`, `<target dir="data_cam"></target><alias name="fs0"/>`, 1)
	got, changed, err = WithShares(withAlias, []Share{cam}, "/usr/libexec/virtiofsd")
	if err != nil || !changed || !strings.Contains(got, `<alias name="fs0"/>`) || strings.Contains(got, "resources") {
		t.Fatalf("kept one untouched, other one cut: %v %v\n%s", changed, err, got)
	}

	// none: every share cut, the shared memory left on, a 9p filesystem left alone
	ninep := strings.Replace(got, "<memballoon", `<filesystem type='mount' accessmode='mapped'><source dir='/DATA/x'/><target dir='x'/></filesystem><memballoon`, 1)
	got, changed, err = WithShares(ninep, nil, "")
	d, _ = ParseDomainXML(got)
	if err != nil || !changed || len(d.Shares) != 0 || !d.SharedMemory || !strings.Contains(got, "<target dir='x'/>") {
		t.Fatalf("none: %v %v %+v\n%s", changed, err, d.Shares, got)
	}

	// a VM with memory of its own is not given shares
	huge := strings.Replace(domainForMemory, "<vcpu", "<memoryBacking><hugepages/></memoryBacking>\n  <vcpu", 1)
	if _, _, err := WithShares(huge, []Share{cam}, ""); !errors.Is(err, ErrMemoryBacking) {
		t.Fatalf("hugepages: %v", err)
	}
}
