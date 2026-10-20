package libvirt

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
)

const testUUID = "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b"

func testDom(t *testing.T, name string) golibvirt.Domain {
	t.Helper()
	return golibvirt.Domain{Name: name, UUID: golibvirt.UUID(uuid.MustParse(testUUID))}
}

func tp(field string, v any) golibvirt.TypedParam {
	return golibvirt.TypedParam{Field: field, Value: golibvirt.TypedParamValue{I: v}}
}

// show prints a counter: "-" when absent.
func show(p *uint64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatUint(*p, 10)
}

func showNIC(n NICStats) string {
	return fmt.Sprintf("%d %s rx %s/%s/%s tx %s/%s/%s", n.Index, n.ID(), show(n.RxBytes), show(n.RxPkts), show(n.RxDrop), show(n.TxBytes), show(n.TxPkts), show(n.TxDrop))
}

func showDisk(d DiskStats) string {
	return fmt.Sprintf("%s rd %s/%s wr %s/%s", d.Name, show(d.RdBytes), show(d.RdReqs), show(d.WrBytes), show(d.WrReqs))
}

func TestAllStatsGroups(t *testing.T) {
	want := uint32(golibvirt.DomainStatsState | golibvirt.DomainStatsCPUTotal | golibvirt.DomainStatsBalloon |
		golibvirt.DomainStatsVCPU | golibvirt.DomainStatsInterface | golibvirt.DomainStatsBlock)
	if AllStatsGroups != want {
		t.Errorf("AllStatsGroups = %d, want %d", AllStatsGroups, want)
	}
	if StatsNoWait != 1<<29 {
		t.Errorf("StatsNoWait = %d", StatsNoWait)
	}
}

func TestDeviceCapsFitTheSeriesCap(t *testing.T) {
	// 7 fixed keys (cpu/total, cpu/vcpus, memory/assigned, memory/used,
	// alert/cpu_high, alert/mem_high, health/ok), 3 per NIC, 4 per disk
	if n := 7 + 3*MaxStatsNICs + 4*MaxStatsDisks; n > 48 {
		t.Errorf("%d series per VM, more than 48", n)
	}
}

func TestParseDomainStatsFull(t *testing.T) {
	s := ParseDomainStats(golibvirt.DomainStatsRecord{Dom: testDom(t, "web"), Params: []golibvirt.TypedParam{
		tp("state.state", int32(1)),
		tp("cpu.time", uint64(12_500_000_000)),
		tp("vcpu.current", uint32(2)),
		tp("balloon.current", uint64(1048576)),
		tp("balloon.available", uint64(1000000)),
		tp("balloon.unused", uint64(400000)),
		tp("balloon.usable", uint64(500000)),
		tp("balloon.rss", uint64(900000)),
		tp("net.count", uint32(2)),
		tp("net.0.name", "vnet3"),
		tp("net.0.rx.bytes", uint64(100)),
		tp("net.0.rx.pkts", uint64(3)),
		tp("net.0.rx.drop", uint64(1)),
		tp("net.0.tx.bytes", uint64(200)),
		tp("net.0.tx.pkts", uint64(4)),
		tp("net.0.tx.drop", uint64(0)),
		tp("net.1.name", "vnet4"), // no counter
		tp("net.5000.rx.bytes", uint64(1)),
		tp("block.count", uint32(2)),
		tp("block.0.name", "vda"),
		tp("block.0.rd.bytes", uint64(10)),
		tp("block.0.wr.bytes", uint64(20)),
		tp("block.0.rd.reqs", uint64(1)),
		tp("block.0.wr.reqs", uint64(2)),
		tp("block.1.name", "sda"),
	}})

	if s.UUID != testUUID || s.Name != "web" || s.State != StateRunning {
		t.Errorf("identity %q %q %q", s.UUID, s.Name, s.State)
	}
	if s.CPUTimeNs == nil || *s.CPUTimeNs != 12_500_000_000 {
		t.Errorf("CPUTimeNs %v", s.CPUTimeNs)
	}
	if s.VCPUs == nil || *s.VCPUs != 2 {
		t.Errorf("VCPUs %v", s.VCPUs)
	}
	if got := show(s.MemAssignedKiB) + " " + show(s.MemAvailableKiB) + " " + show(s.MemUnusedKiB) + " " + show(s.MemUsableKiB); got != "1048576 1000000 400000 500000" {
		t.Errorf("memory %s", got)
	}
	if len(s.NICs) != 1 || showNIC(s.NICs[0]) != "0 nic0 rx 100/3/1 tx 200/4/0" {
		t.Errorf("NICs %d %v", len(s.NICs), s.NICs)
	}
	if len(s.Disks) != 1 || showDisk(s.Disks[0]) != "vda rd 10/1 wr 20/2" {
		t.Errorf("Disks %+v", s.Disks)
	}
}

func TestParseDomainStatsPartialCounters(t *testing.T) {
	// an OVS or vhost-user NIC: libvirt leaves out the counters it lacks
	first := ParseDomainStats(golibvirt.DomainStatsRecord{Params: []golibvirt.TypedParam{
		tp("net.0.rx.bytes", uint64(100)), tp("net.0.tx.bytes", uint64(200)),
		tp("block.0.name", "vda"), tp("block.0.rd.reqs", uint64(7)),
	}})
	if len(first.NICs) != 1 || showNIC(first.NICs[0]) != "0 nic0 rx 100/-/- tx 200/-/-" {
		t.Errorf("first NICs %v", first.NICs)
	}
	if len(first.Disks) != 1 || showDisk(first.Disks[0]) != "vda rd -/7 wr -/-" {
		t.Errorf("first disks %v", first.Disks)
	}

	// a counter that appears later is present from then on
	second := ParseDomainStats(golibvirt.DomainStatsRecord{Params: []golibvirt.TypedParam{
		tp("net.0.rx.bytes", uint64(150)), tp("net.0.tx.bytes", uint64(250)), tp("net.0.rx.drop", uint64(2)),
	}})
	if len(second.NICs) != 1 || showNIC(second.NICs[0]) != "0 nic0 rx 150/-/2 tx 250/-/-" {
		t.Errorf("second NICs %v", second.NICs)
	}
}

func TestParseDomainStatsShutOff(t *testing.T) {
	// what QEMU answers for a shut-off domain with flags 0
	s := ParseDomainStats(golibvirt.DomainStatsRecord{Dom: testDom(t, "off"), Params: []golibvirt.TypedParam{
		tp("state.state", int32(5)),
		tp("state.reason", int32(1)),
		tp("balloon.current", uint64(2097152)),
		tp("balloon.maximum", uint64(2097152)),
		tp("vcpu.current", uint32(2)),
		tp("vcpu.maximum", uint32(2)),
		tp("block.count", uint32(2)),
		tp("block.0.name", "vda"),
		tp("block.0.path", "/DATA/VM/a.qcow2"),
		tp("block.1.name", "sda"),
	}})

	if s.State != StateShutOff {
		t.Errorf("State %q", s.State)
	}
	if s.MemAssignedKiB == nil || *s.MemAssignedKiB != 2097152 || s.VCPUs == nil || *s.VCPUs != 2 {
		t.Errorf("gauges %s %v", show(s.MemAssignedKiB), s.VCPUs)
	}
	if s.CPUTimeNs != nil || s.MemAvailableKiB != nil || s.MemUnusedKiB != nil || s.MemUsableKiB != nil {
		t.Errorf("counters set: %+v", s)
	}
	if len(s.NICs) != 0 || len(s.Disks) != 0 {
		t.Errorf("devices %+v %+v", s.NICs, s.Disks)
	}
}

func TestParseDomainStatsNoState(t *testing.T) {
	if s := ParseDomainStats(golibvirt.DomainStatsRecord{Dom: testDom(t, "x")}); s.State != StateUnknown {
		t.Errorf("State %q", s.State)
	}
	if s := ParseDomainStats(golibvirt.DomainStatsRecord{Dom: testDom(t, "x"), Params: []golibvirt.TypedParam{tp("state.state", uint64(1<<32+1))}}); s.State != StateUnknown {
		t.Errorf("huge state: %q", s.State)
	}
}

func TestParseDomainStatsValueTypes(t *testing.T) {
	for _, v := range []any{int32(42), uint32(42), int64(42), uint64(42)} {
		s := ParseDomainStats(golibvirt.DomainStatsRecord{Params: []golibvirt.TypedParam{tp("cpu.time", v)}})
		if s.CPUTimeNs == nil || *s.CPUTimeNs != 42 {
			t.Errorf("%T: %v", v, s.CPUTimeNs)
		}
	}
	for _, v := range []any{int64(-1), int32(-5), "42", float64(42), nil} {
		s := ParseDomainStats(golibvirt.DomainStatsRecord{Params: []golibvirt.TypedParam{tp("cpu.time", v), tp("balloon.current", v)}})
		if s.CPUTimeNs != nil || s.MemAssignedKiB != nil {
			t.Errorf("%T %v counts: %v %v", v, v, s.CPUTimeNs, s.MemAssignedKiB)
		}
	}

	s := ParseDomainStats(golibvirt.DomainStatsRecord{Params: []golibvirt.TypedParam{tp("cpu.time", uint64(math.MaxUint64))}})
	if s.CPUTimeNs == nil || *s.CPUTimeNs != math.MaxUint64 {
		t.Errorf("huge counter %v", s.CPUTimeNs)
	}

	s = ParseDomainStats(golibvirt.DomainStatsRecord{Params: []golibvirt.TypedParam{tp("vcpu.current", uint64(math.MaxUint64))}})
	if s.VCPUs != nil {
		t.Errorf("huge vcpus %d", *s.VCPUs)
	}

	// a negative counter on a device reads as not reported
	s = ParseDomainStats(golibvirt.DomainStatsRecord{Params: []golibvirt.TypedParam{
		tp("block.0.name", "vda"), tp("block.0.rd.bytes", int64(-1)), tp("block.0.wr.bytes", int64(-1)),
	}})
	if len(s.Disks) != 0 {
		t.Errorf("disk with negative counters only %+v", s.Disks)
	}
}

func TestParseDomainStatsHostileText(t *testing.T) {
	long := strings.Repeat("é", 300)
	s := ParseDomainStats(golibvirt.DomainStatsRecord{Dom: golibvirt.Domain{Name: "a\x1b[31mb\nc\x00" + long + "\xff"}, Params: []golibvirt.TypedParam{
		tp("block.0.name", "v\x07d/a"), tp("block.0.rd.bytes", uint64(1)),
		tp("block.1.name", "\x01\x02"), tp("block.1.rd.bytes", uint64(1)),
		tp("block.2.rd.bytes", uint64(1)), // no name
		tp("block.3.name", strings.Repeat("x", 100)), tp("block.3.rd.bytes", uint64(1)),
		tp("block.4.name", "vda"), tp("block.4.rd.bytes", uint64(9)), // same name again
		tp("block.-1.name", "vdz"), tp("block.-1.rd.bytes", uint64(1)),
		tp("block.x.name", "vdy"), tp("block.x.rd.bytes", uint64(1)),
		tp("net.99999999999999999999.rx.bytes", uint64(1)),
		tp("net.0", uint64(1)),
	}})

	if strings.ContainsAny(s.Name, "\x1b\n\x00\xff") || !strings.HasPrefix(s.Name, "a[31mbc") || len(s.Name) > maxNameLen {
		t.Errorf("Name %q (%d bytes)", s.Name, len(s.Name))
	}
	var names []string
	for _, d := range s.Disks {
		names = append(names, d.Name)
	}
	if got := strings.Join(names, ","); got != "vda,"+strings.Repeat("x", maxDeviceNameLen) {
		t.Errorf("disks %q", got)
	}
	if show(s.Disks[0].RdBytes) != "1" {
		t.Errorf("first vda wins: %s", showDisk(s.Disks[0]))
	}
	if len(s.NICs) != 0 {
		t.Errorf("NICs %+v", s.NICs)
	}
}

func TestParseDomainStatsCapsDevicesInStableOrder(t *testing.T) {
	var params []golibvirt.TypedParam
	for i := 9; i >= 0; i-- {
		params = append(params, tp(fmt.Sprintf("net.%d.rx.bytes", i), uint64(i)))
	}
	for _, d := range []string{"vdj", "vdb", "sda", "vdi", "vda", "vdh", "vdc", "vdg", "vdd", "vdf", "vde"} {
		params = append(params, tp("block."+strconv.Itoa(len(params))+".name", d), tp("block."+strconv.Itoa(len(params))+".rd.bytes", uint64(1)))
	}

	s := ParseDomainStats(golibvirt.DomainStatsRecord{Params: params})

	if len(s.NICs) != MaxStatsNICs {
		t.Fatalf("%d NICs", len(s.NICs))
	}
	for i, n := range s.NICs {
		if n.Index != i {
			t.Errorf("NIC %d has index %d", i, n.Index)
		}
	}
	var names []string
	for _, d := range s.Disks {
		names = append(names, d.Name)
	}
	if got := strings.Join(names, ","); got != "sda,vda,vdb,vdc,vdd,vde,vdf" {
		t.Errorf("disks %q", got)
	}
}

func TestIsNoSupport(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("x: %w", golibvirt.Error{Code: uint32(golibvirt.ErrNoSupport)}), true},
		{golibvirt.Error{Code: uint32(golibvirt.ErrOperationUnsupported)}, true},
		{golibvirt.Error{Code: uint32(golibvirt.ErrNoDomain)}, false},
		{errors.New("no support"), false},
		{nil, false},
	} {
		if got := IsNoSupport(c.err); got != c.want {
			t.Errorf("IsNoSupport(%v) = %v", c.err, got)
		}
	}
}

func TestNICStatsID(t *testing.T) {
	if id := (NICStats{Index: 2}).ID(); id != "nic2" {
		t.Errorf("no MAC: %q", id)
	}
	if id := (NICStats{Index: 2, MAC: "52:54:00:aa:bb:cc"}).ID(); id != "52:54:00:aa:bb:cc" {
		t.Errorf("MAC: %q", id)
	}
}

// fakeLibvirt answers the stats calls from fixed data, keyed by domain name.
type fakeLibvirt struct {
	records   []golibvirt.DomainStatsRecord
	allErr    error
	noNoWait  bool  // refuses StatsNoWait as an old libvirt does
	noWaitErr error // returned instead when set
	allCalls  int
	allArgs   [][2]uint32
	inFlight  atomic.Int32
	maxIn     atomic.Int32
	delay     time.Duration

	doms    []golibvirt.Domain
	listErr error
	info    map[string][5]uint64 // state, maxMem, memory, vcpus, cpuTime
	infoErr map[string]error
	mem     map[string][]golibvirt.DomainMemoryStat
	memErr  map[string]error
	xml     map[string]string
	xmlErr  map[string]error
	xmlCall int
	block   map[string][4]int64 // rdReq, rdBytes, wrReq, wrBytes, by "dom/target"
	iface   map[string][6]int64 // rxBytes, rxPkts, rxDrop, txBytes, txPkts, txDrop, by "dom/mac"
	devErr  map[string]error    // by "dom/device"
}

func (f *fakeLibvirt) ConnectGetAllDomainStats(doms []golibvirt.Domain, groups, flags uint32) ([]golibvirt.DomainStatsRecord, error) {
	if n := f.inFlight.Add(1); n > f.maxIn.Load() {
		f.maxIn.Store(n)
	}
	defer f.inFlight.Add(-1)
	time.Sleep(f.delay)

	f.allCalls++
	f.allArgs = append(f.allArgs, [2]uint32{groups, flags})
	if doms != nil {
		return nil, errors.New("doms should be nil")
	}
	if f.noWaitErr != nil && flags&StatsNoWait != 0 {
		return nil, f.noWaitErr
	}
	if f.noNoWait && flags&StatsNoWait != 0 {
		return nil, golibvirt.Error{Code: uint32(golibvirt.ErrInvalidArg), Message: "unsupported flags (0x20000000) in function virConnectGetAllDomainStats"}
	}
	return f.records, f.allErr
}

func (f *fakeLibvirt) ConnectListAllDomains(need int32, flags golibvirt.ConnectListAllDomainsFlags) ([]golibvirt.Domain, uint32, error) {
	return f.doms, uint32(len(f.doms)), f.listErr
}

func (f *fakeLibvirt) DomainGetInfo(d golibvirt.Domain) (uint8, uint64, uint64, uint16, uint64, error) {
	if err := f.infoErr[d.Name]; err != nil {
		return 0, 0, 0, 0, 0, err
	}
	i := f.info[d.Name]
	return uint8(i[0]), i[1], i[2], uint16(i[3]), i[4], nil
}

func (f *fakeLibvirt) DomainMemoryStats(d golibvirt.Domain, max, flags uint32) ([]golibvirt.DomainMemoryStat, error) {
	return f.mem[d.Name], f.memErr[d.Name]
}

func (f *fakeLibvirt) DomainGetXMLDesc(d golibvirt.Domain, flags golibvirt.DomainXMLFlags) (string, error) {
	f.xmlCall++
	return f.xml[d.Name], f.xmlErr[d.Name]
}

func (f *fakeLibvirt) DomainBlockStats(d golibvirt.Domain, path string) (int64, int64, int64, int64, int64, error) {
	if err := f.devErr[d.Name+"/"+path]; err != nil {
		return 0, 0, 0, 0, 0, err
	}
	b, ok := f.block[d.Name+"/"+path]
	if !ok {
		return 0, 0, 0, 0, 0, golibvirt.Error{Code: uint32(golibvirt.ErrInvalidArg)}
	}
	return b[0], b[1], b[2], b[3], 0, nil
}

func (f *fakeLibvirt) DomainInterfaceStats(d golibvirt.Domain, dev string) (int64, int64, int64, int64, int64, int64, int64, int64, error) {
	if err := f.devErr[d.Name+"/"+dev]; err != nil {
		return 0, 0, 0, 0, 0, 0, 0, 0, err
	}
	n, ok := f.iface[d.Name+"/"+dev]
	if !ok {
		return 0, 0, 0, 0, 0, 0, 0, 0, golibvirt.Error{Code: uint32(golibvirt.ErrInvalidArg)}
	}
	return n[0], n[1], 0, n[2], n[3], n[4], 0, n[5], nil
}

func lerr(code golibvirt.ErrorNumber) error { return golibvirt.Error{Code: uint32(code), Message: "x"} }

const webXML = `<domain><name>web</name><devices>
<disk type='file' device='disk'><source file='/DATA/a.qcow2'/><target dev='vda'/></disk>
<disk type='file' device='cdrom'><target dev='sda'/></disk>
<disk type='block' device='disk'><target dev='sdb'/></disk>
<interface type='network'><mac address='52:54:00:AA:BB:CC'/><source network='casaos'/><target dev='vnet3'/></interface>
<interface type='network'><source network='casaos'/></interface>
<interface type='network'><mac address='52:54:00:00:00:01'/><source network='casaos'/></interface>
<interface type='network'><mac address='52:54:00:00:00:02'/><source network='casaos'/></interface>
</devices></domain>`

func webRecord(t *testing.T, ifname string, cpu uint64) golibvirt.DomainStatsRecord {
	return golibvirt.DomainStatsRecord{Dom: testDom(t, "web"), Params: []golibvirt.TypedParam{
		tp("state.state", int32(1)), tp("cpu.time", cpu),
		tp("net.0.name", ifname), tp("net.0.rx.bytes", uint64(5)),
		tp("net.1.name", "vnet9"), tp("net.1.rx.bytes", uint64(6)),
	}}
}

func TestStatsReaderOneCallWithStableNICIDs(t *testing.T) {
	f := &fakeLibvirt{records: []golibvirt.DomainStatsRecord{webRecord(t, "vnet3", 100)}, xml: map[string]string{"web": webXML}}
	var r StatsReader

	got, err := r.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	if f.allCalls != 1 || f.allArgs[0] != [2]uint32{AllStatsGroups, StatsNoWait} {
		t.Errorf("calls %d %v", f.allCalls, f.allArgs)
	}
	if len(got) != 1 || len(got[0].NICs) != 2 || got[0].NICs[0].ID() != "52:54:00:aa:bb:cc" || got[0].NICs[1].ID() != "nic1" {
		t.Fatalf("got %+v", got)
	}

	// same host devices: the XML is not read again; counters that went
	// backwards are reported as they are
	f.records = []golibvirt.DomainStatsRecord{webRecord(t, "vnet3", 50)}
	got, _ = r.Read(f)
	if f.xmlCall != 1 || *got[0].CPUTimeNs != 50 || got[0].NICs[0].ID() != "52:54:00:aa:bb:cc" {
		t.Errorf("xml calls %d, %+v", f.xmlCall, got)
	}

	// the VM restarted on another tap device: the XML is read again
	f.records = []golibvirt.DomainStatsRecord{webRecord(t, "vnet7", 1)}
	f.xml["web"] = strings.Replace(webXML, "52:54:00:AA:BB:CC", "52:54:00:dd:ee:ff", 1)
	got, _ = r.Read(f)
	if f.xmlCall != 2 || got[0].NICs[0].ID() != "52:54:00:dd:ee:ff" {
		t.Errorf("xml calls %d, %+v", f.xmlCall, got[0].NICs)
	}
}

func TestStatsReaderNoWaitRefused(t *testing.T) {
	f := &fakeLibvirt{noNoWait: true, records: []golibvirt.DomainStatsRecord{{Dom: testDom(t, "x")}}}
	var r StatsReader

	for range 2 {
		if got, err := r.Read(f); err != nil || len(got) != 1 {
			t.Fatalf("%+v %v", got, err)
		}
	}
	if fmt.Sprint(f.allArgs) != fmt.Sprintf("[[63 %d] [63 0] [63 0]]", StatsNoWait) {
		t.Errorf("calls %v", f.allArgs)
	}

	r.Reset()
	_, _ = r.Read(f)
	if f.allArgs[3][1] != StatsNoWait {
		t.Errorf("after Reset %v", f.allArgs)
	}
}

func TestStatsReaderNoWaitRefusedCodes(t *testing.T) {
	for _, code := range []golibvirt.ErrorNumber{golibvirt.ErrInvalidArg, golibvirt.ErrNoSupport, golibvirt.ErrOperationInvalid, golibvirt.ErrArgumentUnsupported} {
		f := &fakeLibvirt{noWaitErr: lerr(code), records: []golibvirt.DomainStatsRecord{{Dom: testDom(t, "x")}}}
		var told []error
		r := StatsReader{OnNoWaitRefused: func(err error) { told = append(told, err) }}

		for range 2 {
			if got, err := r.Read(f); err != nil || len(got) != 1 {
				t.Fatalf("%d: %+v %v", code, got, err)
			}
		}
		if fmt.Sprint(f.allArgs) != fmt.Sprintf("[[63 %d] [63 0] [63 0]]", StatsNoWait) || len(told) != 1 {
			t.Errorf("%d: calls %v, told %v", code, f.allArgs, told)
		}
	}

	// any other error is not a refusal of the flag
	f := &fakeLibvirt{noWaitErr: lerr(golibvirt.ErrInternalError)}
	var r StatsReader
	if _, err := r.Read(f); err == nil || len(f.allArgs) != 1 {
		t.Errorf("internal error: %v, calls %v", err, f.allArgs)
	}

	// the retry failing too: not remembered, the flag is asked again next time
	f = &fakeLibvirt{noWaitErr: lerr(golibvirt.ErrInvalidArg), allErr: lerr(golibvirt.ErrInternalError)}
	r = StatsReader{}
	_, _ = r.Read(f)
	_, _ = r.Read(f)
	if fmt.Sprint(f.allArgs) != fmt.Sprintf("[[63 %d] [63 0] [63 %d] [63 0]]", StatsNoWait, StatsNoWait) {
		t.Errorf("failed retry: calls %v", f.allArgs)
	}
}

func TestStatsReaderUnreadableXMLDropsNICs(t *testing.T) {
	f := &fakeLibvirt{records: []golibvirt.DomainStatsRecord{webRecord(t, "vnet3", 100)}, xmlErr: map[string]error{"web": lerr(golibvirt.ErrNoDomain)}}
	var r StatsReader

	got, err := r.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].NICs) != 0 || got[0].CPUTimeNs == nil {
		t.Fatalf("got %+v", got)
	}

	// not cached: the next read tries again
	delete(f.xmlErr, "web")
	f.xml = map[string]string{"web": webXML}
	got, _ = r.Read(f)
	if len(got[0].NICs) != 2 {
		t.Errorf("NICs %+v", got[0].NICs)
	}
}

func TestStatsReaderReturnsOtherErrors(t *testing.T) {
	for _, e := range []error{errors.New("connection reset"), lerr(golibvirt.ErrInternalError)} {
		f := &fakeLibvirt{allErr: e}
		fallbacks := 0
		r := StatsReader{OnFallback: func(error) { fallbacks++ }}

		if _, err := r.Read(f); !errors.Is(err, e) {
			t.Errorf("%v: got %v", e, err)
		}
		_, _ = r.Read(f)
		if f.allCalls != 2 || fallbacks != 0 {
			t.Errorf("%v: %d calls, %d fallbacks", e, f.allCalls, fallbacks)
		}
	}
}

func TestStatsReaderFallsBackOncePerConnection(t *testing.T) {
	for _, code := range []golibvirt.ErrorNumber{golibvirt.ErrNoSupport, golibvirt.ErrOperationUnsupported, golibvirt.ErrOperationDenied, golibvirt.ErrArgumentUnsupported} {
		f := &fakeLibvirt{allErr: fmt.Errorf("stats: %w", lerr(code)), doms: []golibvirt.Domain{testDom(t, "off")}, info: map[string][5]uint64{"off": {5}}}
		var reasons []error
		r := StatsReader{OnFallback: func(err error) { reasons = append(reasons, err) }}

		for range 3 {
			got, err := r.Read(f)
			if err != nil || len(got) != 1 || got[0].State != StateShutOff {
				t.Fatalf("%v: %+v %v", code, got, err)
			}
		}
		// a refusal that may be of StatsNoWait is first asked again without it
		calls := 1
		if code == golibvirt.ErrNoSupport || code == golibvirt.ErrArgumentUnsupported {
			calls = 2
		}
		if f.allCalls != calls || len(reasons) != 1 {
			t.Errorf("%v: %d calls, fallbacks %v", code, f.allCalls, reasons)
		}

		// a new connection is probed again
		r.Reset()
		f.allErr, f.records = nil, []golibvirt.DomainStatsRecord{{Dom: testDom(t, "x")}}
		if got, err := r.Read(f); err != nil || len(got) != 1 || got[0].Name != "x" || f.allCalls != calls+1 {
			t.Errorf("%v: after Reset %+v %v, %d calls", code, got, err, f.allCalls)
		}
	}
}

// The callbacks run after the reader's lock is released: one may call Reset
// (or Read) without a deadlock.
func TestStatsReaderCallbacksMayReset(t *testing.T) {
	var r StatsReader
	r.OnFallback = func(error) { r.Reset() }
	r.OnNoWaitRefused = func(error) { r.Reset() }
	cases := map[string]*fakeLibvirt{
		"fallback": {allErr: lerr(golibvirt.ErrOperationDenied), doms: []golibvirt.Domain{testDom(t, "off")}, info: map[string][5]uint64{"off": {5}}},
		"no wait":  {noNoWait: true, records: []golibvirt.DomainStatsRecord{{Dom: testDom(t, "x")}}},
	}
	for name, f := range cases {
		done := make(chan error, 1)
		go func() {
			_, err := r.Read(f)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: Read deadlocked in its callback", name)
		}
	}
}

func TestStatsReaderConcurrentReads(t *testing.T) {
	f := &fakeLibvirt{records: []golibvirt.DomainStatsRecord{webRecord(t, "vnet3", 1)}, xml: map[string]string{"web": webXML}, delay: time.Millisecond}
	var r StatsReader

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 5 {
				if _, err := r.Read(f); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Go(func() {
		for range 5 {
			r.Reset()
			time.Sleep(time.Millisecond)
		}
	})
	wg.Wait()

	if f.maxIn.Load() != 1 || f.allCalls != 40 {
		t.Errorf("%d reads at once, %d calls", f.maxIn.Load(), f.allCalls)
	}
}

func TestInfoDomainStats(t *testing.T) {
	gone := testDom(t, "gone")
	f := &fakeLibvirt{
		doms: []golibvirt.Domain{testDom(t, "web"), testDom(t, "off"), gone, testDom(t, "late"), testDom(t, "plain")},
		info: map[string][5]uint64{
			"web":   {1, 4194304, 2097152, 2, 7_000_000_000},
			"off":   {5, 4194304, 4194304, 2, 0},
			"late":  {1, 1024, 1024, 1, 1},
			"plain": {3, 1024, 1024, 1, 9},
		},
		infoErr: map[string]error{"gone": lerr(golibvirt.ErrNoDomain)},
		mem: map[string][]golibvirt.DomainMemoryStat{"web": {
			{Tag: int32(golibvirt.DomainMemoryStatAvailable), Val: 2000000},
			{Tag: int32(golibvirt.DomainMemoryStatUnused), Val: 500000},
			{Tag: int32(golibvirt.DomainMemoryStatUsable), Val: 600000},
			{Tag: int32(golibvirt.DomainMemoryStatRss), Val: 1},
		}},
		memErr: map[string]error{"late": lerr(golibvirt.ErrNoDomain), "plain": lerr(golibvirt.ErrNoSupport)},
		xml:    map[string]string{"web": webXML},
		xmlErr: map[string]error{"plain": lerr(golibvirt.ErrInternalError)},
		block:  map[string][4]int64{"web/vda": {1, 512, -1, 1024}, "web/sdb": {-1, -1, -1, -1}},
		iface:  map[string][6]int64{"web/52:54:00:AA:BB:CC": {10, 1, 0, 20, 2, -1}, "web/52:54:00:00:00:01": {-1, -1, -1, -1, -1, -1}},
		devErr: map[string]error{"web/sda": lerr(golibvirt.ErrOperationInvalid)},
	}

	got, err := InfoDomainStats(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "web" || got[1].Name != "off" || got[2].Name != "plain" {
		t.Fatalf("got %+v", got)
	}

	web := got[0]
	if web.UUID != testUUID || web.State != StateRunning || *web.CPUTimeNs != 7_000_000_000 || *web.VCPUs != 2 || *web.MemAssignedKiB != 2097152 {
		t.Errorf("web %+v", web)
	}
	if got := show(web.MemAvailableKiB) + " " + show(web.MemUnusedKiB) + " " + show(web.MemUsableKiB); got != "2000000 500000 600000" {
		t.Errorf("web balloon %s", got)
	}
	// -1 is not reported
	if len(web.Disks) != 1 || showDisk(web.Disks[0]) != "vda rd 512/1 wr 1024/-" {
		t.Errorf("web disks %v", web.Disks)
	}
	if len(web.NICs) != 1 || showNIC(web.NICs[0]) != "0 52:54:00:aa:bb:cc rx 10/1/0 tx 20/2/-" {
		t.Errorf("web NICs %v", web.NICs)
	}

	// the shape a shut-off domain has in ConnectGetAllDomainStats
	off := got[1]
	if off.State != StateShutOff || off.CPUTimeNs != nil || show(off.MemAssignedKiB) != "4194304" || off.VCPUs == nil || *off.VCPUs != 2 || len(off.NICs)+len(off.Disks) != 0 {
		t.Errorf("off %+v", off)
	}

	plain := got[2]
	if plain.State != StatePaused || *plain.CPUTimeNs != 9 || plain.MemAvailableKiB != nil || len(plain.NICs)+len(plain.Disks) != 0 {
		t.Errorf("plain %+v", plain)
	}
}

func TestInfoDomainStatsFailsOnErrors(t *testing.T) {
	down := errors.New("connection reset")
	internal := lerr(golibvirt.ErrInternalError)
	base := func() *fakeLibvirt {
		return &fakeLibvirt{
			doms: []golibvirt.Domain{testDom(t, "ok"), testDom(t, "web")},
			info: map[string][5]uint64{"ok": {5}, "web": {1, 1, 1, 1, 1}},
			xml:  map[string]string{"web": webXML},
		}
	}
	for name, c := range map[string]struct {
		set  func(*fakeLibvirt)
		want error
	}{
		"list":          {func(f *fakeLibvirt) { f.listErr = down }, down},
		"info":          {func(f *fakeLibvirt) { f.infoErr = map[string]error{"web": down} }, down},
		"info internal": {func(f *fakeLibvirt) { f.infoErr = map[string]error{"web": internal} }, internal},
		"mem":           {func(f *fakeLibvirt) { f.memErr = map[string]error{"web": down} }, down},
		"xml":           {func(f *fakeLibvirt) { f.xmlErr = map[string]error{"web": down} }, down},
		"block":         {func(f *fakeLibvirt) { f.devErr = map[string]error{"web/vda": down} }, down},
		"iface":         {func(f *fakeLibvirt) { f.devErr = map[string]error{"web/52:54:00:AA:BB:CC": down} }, down},
	} {
		l := base()
		c.set(l)
		// the whole reading fails, the other domain with it
		if got, err := InfoDomainStats(l); !errors.Is(err, c.want) || got != nil {
			t.Errorf("%s: %v %+v", name, err, got)
		}
	}
}
