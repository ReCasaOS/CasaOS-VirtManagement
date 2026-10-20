package vmstats

import (
	"fmt"
	"testing"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
)

// BenchmarkTick10VMs is one tick over 10 running VMs (4 vCPUs, 2 NICs, 2 disks
// each) on a fake source and clock, with a real Store: parsing, maths and
// Record, not the libvirt RPC.
func BenchmarkTick10VMs(b *testing.B) {
	clock := stats.NewFakeClock(base)
	src := &fakeSource{titles: map[string][2]string{}, descCalls: map[string]int{}}
	logError = func(string, error) {}
	logInfo = func(string, error) {}
	s, err := New(Options{Dir: b.TempDir(), Clock: clock, Source: src, Publisher: &fakePublisher{flushed: make(chan struct{})}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })

	const vms = 10
	ids := make([]string, vms)
	for i := range ids {
		ids[i] = fmt.Sprintf("%08d-0000-0000-0000-000000000000", i+1)
		src.titles[ids[i]] = [2]string{"vm", "detail"}
	}
	step := uint64(Interval.Nanoseconds()) // 4 vCPUs at 25 % of a core each
	var tick uint64
	doms := make([]libvirt.DomainStats, vms)
	snapshot := func() {
		tick++
		for i, id := range ids {
			d := libvirt.DomainStats{
				UUID: id, Name: "vm-" + id[:8], State: libvirt.StateRunning,
				CPUTimeNs: u(tick * step), VCPUs: n(4), MemAssignedKiB: u(4194304),
				MemAvailableKiB: u(4194304), MemUnusedKiB: u(1048576),
			}
			for j := range 2 {
				d.NICs = append(d.NICs, nic(fmt.Sprintf("52:54:00:00:%02x:%02x", i, j), j, tick*1000, tick*2000, 0, 0))
			}
			for _, name := range []string{"vda", "vdb"} {
				d.Disks = append(d.Disks, disk(name, tick*4096, tick, tick*8192, tick*2))
			}
			doms[i] = d
		}
		src.set(doms, nil)
	}
	snapshot()
	s.Tick() // the first tick describes the VMs and has no previous reading
	clock.Advance(Interval)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		snapshot()
		b.StartTimer()
		s.Tick()
		b.StopTimer()
		clock.Advance(Interval)
		b.StartTimer()
	}
}
