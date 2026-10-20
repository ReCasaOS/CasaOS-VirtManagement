package vmstats

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
)

var t0 = time.Unix(1_700_000_000, 0)

func u(v uint64) *uint64 { return &v }
func n(v int) *int       { return &v }

func reading(at time.Time, s libvirt.DomainStats) Reading { return Reading{At: at, S: s} }

func running(cpuNs uint64, vcpus int) libvirt.DomainStats {
	return libvirt.DomainStats{State: libvirt.StateRunning, CPUTimeNs: u(cpuNs), VCPUs: n(vcpus), MemAssignedKiB: u(1048576)}
}

const mac0 = "52:54:00:00:00:01"

func nic(mac string, index int, rx, tx, rxDrop, txDrop uint64) libvirt.NICStats {
	return libvirt.NICStats{Index: index, MAC: mac, RxBytes: u(rx), TxBytes: u(tx), RxDrop: u(rxDrop), TxDrop: u(txDrop)}
}

func disk(name string, rd, rdReq, wr, wrReq uint64) libvirt.DiskStats {
	return libvirt.DiskStats{Name: name, RdBytes: u(rd), RdReqs: u(rdReq), WrBytes: u(wr), WrReqs: u(wrReq)}
}

func want(t *testing.T, s stats.Sample, key string, v float64) {
	t.Helper()
	got, ok := s[key]
	if !ok {
		t.Errorf("%s absent, want %v (sample %v)", key, v, s)
	} else if math.Abs(got-v) > 1e-9 {
		t.Errorf("%s = %v, want %v", key, got, v)
	}
}

func absent(t *testing.T, s stats.Sample, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if v, ok := s[k]; ok {
			t.Errorf("%s = %v, want absent", k, v)
		}
	}
}

func noPrefix(t *testing.T, s stats.Sample, prefixes ...string) {
	t.Helper()
	for k := range s {
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				t.Errorf("%s present, want no %s* key", k, p)
			}
		}
	}
}

func TestVMSampleCPUPercent(t *testing.T) {
	prev := reading(t0, running(10e9, 2))
	s, _ := Sample(&prev, reading(t0.Add(5*time.Second), running(12.5e9, 2)))

	want(t, s, "cpu/total", 50)
	want(t, s, "alert/cpu_high", 25)
	want(t, s, "cpu/vcpus", 2)
}

func TestVMSampleVCPUsChange(t *testing.T) {
	prev := reading(t0, running(10e9, 2))
	s, _ := Sample(&prev, reading(t0.Add(5*time.Second), running(12.5e9, 4)))

	want(t, s, "cpu/total", 50)
	want(t, s, "alert/cpu_high", 12.5) // of the current allocation
	want(t, s, "cpu/vcpus", 4)

	cur := running(12.5e9, 0)
	s, _ = Sample(&prev, reading(t0.Add(5*time.Second), cur))
	want(t, s, "cpu/total", 50)
	absent(t, s, "alert/cpu_high")

	cur.VCPUs = nil
	s, _ = Sample(&prev, reading(t0.Add(5*time.Second), cur))
	absent(t, s, "alert/cpu_high", "cpu/vcpus")
}

func TestVMSampleFirstReadingHasNoRates(t *testing.T) {
	cur := running(12.5e9, 2)
	cur.NICs = []libvirt.NICStats{nic(mac0, 0, 1, 1, 1, 1)}
	cur.Disks = []libvirt.DiskStats{disk("vda", 1, 1, 1, 1)}
	s, _ := Sample(nil, reading(t0, cur))

	absent(t, s, "cpu/total", "alert/cpu_high")
	noPrefix(t, s, "network/", "disk/")
	want(t, s, "cpu/vcpus", 2)
	want(t, s, "memory/assigned", 1048576*1024)
}

func TestVMSampleCounterReset(t *testing.T) {
	p := running(500e9, 2)
	p.NICs = []libvirt.NICStats{nic(mac0, 0, 9000, 9000, 50, 50)}
	p.Disks = []libvirt.DiskStats{disk("vda", 9000, 90, 9000, 90)}
	c := running(1e9, 2) // the VM restarted: everything from 0
	c.NICs = []libvirt.NICStats{nic(mac0, 0, 100, 9100, 0, 60)}
	c.Disks = []libvirt.DiskStats{disk("vda", 100, 1, 9500, 95)}

	prev := reading(t0, p)
	s, _ := Sample(&prev, reading(t0.Add(5*time.Second), c))

	if _, ok := s["cpu/total"]; ok {
		t.Errorf("cpu/total present after a reset: %v", s["cpu/total"])
	}
	absent(t, s, "alert/cpu_high", "network/rx:"+mac0, "network/dropped:"+mac0, "disk/read:vda", "disk/iops_read:vda")
	// the counters that went on still tick
	want(t, s, "network/tx:"+mac0, 20)
	want(t, s, "disk/write:vda", 100)
	want(t, s, "disk/iops_write:vda", 1)

	// the next interval resumes
	prev = reading(t0.Add(5*time.Second), c)
	c2 := running(3.5e9, 2)
	c2.NICs = []libvirt.NICStats{nic(mac0, 0, 5100, 9100, 0, 60)}
	s, _ = Sample(&prev, reading(t0.Add(10*time.Second), c2))
	want(t, s, "cpu/total", 50)
	want(t, s, "network/rx:"+mac0, 1000)
}

func TestVMSampleGap(t *testing.T) {
	p := running(10e9, 2)
	p.NICs = []libvirt.NICStats{nic(mac0, 0, 0, 0, 0, 0)}
	p.Disks = []libvirt.DiskStats{disk("vda", 0, 0, 0, 0)}
	c := running(70e9, 2)
	c.NICs = []libvirt.NICStats{nic(mac0, 0, 6000, 6000, 1, 1)}
	c.Disks = []libvirt.DiskStats{disk("vda", 6000, 6, 6000, 6)}
	prev := reading(t0, p)

	s, _ := Sample(&prev, reading(t0.Add(60*time.Second), c))
	absent(t, s, "cpu/total", "alert/cpu_high")
	noPrefix(t, s, "network/", "disk/")
	want(t, s, "cpu/vcpus", 2)
	want(t, s, "memory/assigned", 1048576*1024)

	// MaxGap itself still counts
	s, _ = Sample(&prev, reading(t0.Add(MaxGap), c))
	want(t, s, "cpu/total", 400)

	// no time, or time going back: no rates, no NaN or Inf
	for _, d := range []time.Duration{0, -5 * time.Second} {
		s, _ = Sample(&prev, reading(t0.Add(d), c))
		absent(t, s, "cpu/total")
		noPrefix(t, s, "network/", "disk/")
		for k, v := range s {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Errorf("%s = %v", k, v)
			}
		}
	}
}

func TestVMSampleNoBalloon(t *testing.T) {
	s, unavailable := Sample(nil, reading(t0, running(1, 2)))
	absent(t, s, "memory/used", "alert/mem_high")
	want(t, s, "memory/assigned", 1048576*1024)
	if !unavailable {
		t.Error("memory used not flagged unavailable")
	}
	prev := reading(t0, running(1, 2))
	withBalloon := running(2, 2)
	withBalloon.MemAvailableKiB, withBalloon.MemUnusedKiB = u(1048576), u(1)
	if _, unavailable := Sample(&prev, reading(t0.Add(Interval), running(2, 2))); !unavailable {
		t.Error("memory used not flagged unavailable with a previous reading")
	}
	if _, unavailable := Sample(&prev, reading(t0.Add(Interval), withBalloon)); unavailable {
		t.Error("memory used flagged unavailable with a balloon")
	}

	// a balloon without a driver in the guest reads zero, or reports only part
	for _, c := range []struct{ avail, unused, usable *uint64 }{
		{u(0), u(0), u(0)},
		{u(0), nil, nil},
		{u(1048576), nil, nil},
		{nil, u(1), u(1)},
		{u(1048576), u(1048576), nil},  // used would be 0
		{u(1048576), nil, u(1048576)},  // used would be 0
		{u(1048576), u(2097152), nil},  // used would be negative
		{u(1048576), nil, u(2097152)},  // used would be negative
		{u(math.MaxUint64), nil, u(0)}, // nonsense: more than assigned
		{u(2 * 1048576), u(0), nil},    // more than assigned
	} {
		st := running(1, 2)
		st.MemAvailableKiB, st.MemUnusedKiB, st.MemUsableKiB = c.avail, c.unused, c.usable
		s, unavailable := Sample(nil, reading(t0, st))
		absent(t, s, "memory/used", "alert/mem_high")
		if !unavailable {
			t.Errorf("%+v: not flagged unavailable", c)
		}
	}
}

func TestVMSampleUsedFromUsable(t *testing.T) {
	st := running(1, 2)
	st.MemAvailableKiB, st.MemUsableKiB, st.MemUnusedKiB = u(1048576), u(262144), u(100)
	s, unavailable := Sample(nil, reading(t0, st))

	want(t, s, "memory/used", 786432*1024)
	want(t, s, "alert/mem_high", 75)
	if unavailable {
		t.Error("flagged unavailable")
	}
}

func TestVMSampleUsedFallsBackToUnused(t *testing.T) {
	st := running(1, 2)
	st.MemAvailableKiB, st.MemUnusedKiB = u(1048576), u(524288)
	s, _ := Sample(nil, reading(t0, st))

	want(t, s, "memory/used", 524288*1024)
	want(t, s, "alert/mem_high", 50)

	// no assigned memory: used stays, no percentage
	st.MemAssignedKiB = nil
	s, _ = Sample(nil, reading(t0, st))
	want(t, s, "memory/used", 524288*1024)
	absent(t, s, "alert/mem_high", "memory/assigned")
}

func TestVMSampleNetworkAndDisk(t *testing.T) {
	p := running(10e9, 2)
	p.NICs = []libvirt.NICStats{nic(mac0, 0, 1000, 2000, 10, 20)}
	p.Disks = []libvirt.DiskStats{disk("vda", 100, 10, 200, 20)}
	c := running(10e9, 2)
	c.NICs = []libvirt.NICStats{nic(mac0, 0, 6000, 4500, 13, 22)}
	c.Disks = []libvirt.DiskStats{disk("vda", 10100, 35, 50200, 70)}
	prev := reading(t0, p)

	s, _ := Sample(&prev, reading(t0.Add(5*time.Second), c))
	want(t, s, "network/rx:"+mac0, 1000)
	want(t, s, "network/tx:"+mac0, 500)
	want(t, s, "network/dropped:"+mac0, 1)
	want(t, s, "disk/read:vda", 2000)
	want(t, s, "disk/write:vda", 10000)
	want(t, s, "disk/iops_read:vda", 5)
	want(t, s, "disk/iops_write:vda", 10)
	want(t, s, "cpu/total", 0) // an idle VM is a real 0
	noPrefix(t, s, "network/rx:nic", "network/tx:nic")
}

func TestVMSampleNICWithoutMACUsesIndex(t *testing.T) {
	p := running(1, 1)
	p.NICs = []libvirt.NICStats{nic("", 2, 0, 0, 0, 0)}
	c := running(1, 1)
	c.NICs = []libvirt.NICStats{nic("", 2, 500, 0, 0, 0)}
	prev := reading(t0, p)

	s, _ := Sample(&prev, reading(t0.Add(5*time.Second), c))
	want(t, s, "network/rx:nic2", 100)
}

func TestVMSampleNICsMatchByMAC(t *testing.T) {
	const mac1 = "52:54:00:00:00:02"
	p := running(1, 1)
	p.NICs = []libvirt.NICStats{nic(mac0, 0, 0, 0, 0, 0), nic(mac1, 1, 1000, 0, 0, 0)}
	c := running(1, 1) // the first NIC was unplugged: mac1 is now at index 0
	c.NICs = []libvirt.NICStats{nic(mac1, 0, 1500, 0, 0, 0)}
	prev := reading(t0, p)

	s, _ := Sample(&prev, reading(t0.Add(5*time.Second), c))
	want(t, s, "network/rx:"+mac1, 100)
	noPrefix(t, s, "network/rx:"+mac0)
}

func TestVMSampleCountersPresentInBothOnly(t *testing.T) {
	p := running(1, 1)
	p.NICs = []libvirt.NICStats{{Index: 0, MAC: mac0, RxBytes: u(0)}}
	p.Disks = []libvirt.DiskStats{{Name: "vda", RdReqs: u(0)}}
	c := running(1, 1) // tx and drops, and bytes read, appear only now
	c.NICs = []libvirt.NICStats{nic(mac0, 0, 500, 7e9, 3, 3)}
	c.Disks = []libvirt.DiskStats{disk("vda", 9e9, 5, 9e9, 5)}
	prev := reading(t0, p)

	s, _ := Sample(&prev, reading(t0.Add(5*time.Second), c))
	want(t, s, "network/rx:"+mac0, 100)
	want(t, s, "disk/iops_read:vda", 1)
	absent(t, s, "network/tx:"+mac0, "network/dropped:"+mac0, "disk/read:vda", "disk/write:vda", "disk/iops_write:vda")

	// one drop counter missing on either side: no dropped rate
	p.NICs[0] = nic(mac0, 0, 0, 0, 0, 0)
	p.NICs[0].TxDrop = nil
	prev = reading(t0, p)
	s, _ = Sample(&prev, reading(t0.Add(5*time.Second), c))
	absent(t, s, "network/dropped:"+mac0)
	c.NICs[0].TxDrop = nil
	p.NICs[0].TxDrop, p.NICs[0].RxDrop = u(0), nil
	prev = reading(t0, p)
	s, _ = Sample(&prev, reading(t0.Add(5*time.Second), c))
	absent(t, s, "network/dropped:"+mac0)

	// CPU time reported only now
	p = running(1, 1)
	p.CPUTimeNs = nil
	prev = reading(t0, p)
	s, _ = Sample(&prev, reading(t0.Add(5*time.Second), running(5e9, 1)))
	absent(t, s, "cpu/total", "alert/cpu_high")
}

func TestVMSampleNewNICMidway(t *testing.T) {
	const mac1 = "52:54:00:00:00:02"
	p := running(1, 1)
	p.NICs = []libvirt.NICStats{nic(mac0, 0, 0, 0, 0, 0)}
	p.Disks = []libvirt.DiskStats{disk("vda", 0, 0, 0, 0)}
	c := running(1, 1)
	c.NICs = []libvirt.NICStats{nic(mac0, 0, 5000, 0, 0, 0), nic(mac1, 1, 9e12, 9e12, 9e9, 9e9)}
	c.Disks = []libvirt.DiskStats{disk("vda", 0, 0, 0, 0), disk("vdb", 9e12, 9e9, 9e12, 9e9)}
	prev := reading(t0, p)

	s, _ := Sample(&prev, reading(t0.Add(5*time.Second), c))
	noPrefix(t, s, "network/rx:"+mac1, "network/tx:"+mac1, "network/dropped:"+mac1, "network/rx:nic1", "disk/read:vdb", "disk/write:vdb", "disk/iops_read:vdb", "disk/iops_write:vdb")
	want(t, s, "network/rx:"+mac0, 1000)
	want(t, s, "disk/read:vda", 0)
}

func TestVMSampleHugeCounters(t *testing.T) {
	big := uint64(math.MaxUint64 - 10)
	p := running(big-5e9, 1)
	p.NICs = []libvirt.NICStats{nic(mac0, 0, big-5000, 1<<63, 0, 0)}
	c := running(big, 1)
	c.NICs = []libvirt.NICStats{nic(mac0, 0, big, (1<<63)+5000, 0, 0)}
	prev := reading(t0, p)

	s, _ := Sample(&prev, reading(t0.Add(5*time.Second), c))
	want(t, s, "cpu/total", 100)
	want(t, s, "network/rx:"+mac0, 1000)
	want(t, s, "network/tx:"+mac0, 1000)

	// KiB to bytes does not wrap around
	st := running(1, 1)
	st.MemAssignedKiB = u(math.MaxUint64)
	s, _ = Sample(nil, reading(t0, st))
	if got := s["memory/assigned"]; got < 1.8e22 || math.IsInf(got, 0) {
		t.Errorf("memory/assigned = %v", got)
	}
}

func TestVMSampleSeriesCap(t *testing.T) {
	p := running(1, 1)
	c := running(2, 1)
	c.MemAvailableKiB, c.MemUsableKiB = u(1048576), u(1)
	for i := range 2 * libvirt.MaxStatsNICs {
		mac := "52:54:00:00:01:0" + string(rune('0'+i))
		p.NICs = append(p.NICs, nic(mac, i, 0, 0, 0, 0))
		c.NICs = append(c.NICs, nic(mac, i, 1, 1, 1, 1))
	}
	for i := range 2 * libvirt.MaxStatsDisks {
		name := "vd" + string(rune('a'+i))
		p.Disks = append(p.Disks, disk(name, 0, 0, 0, 0))
		c.Disks = append(c.Disks, disk(name, 1, 1, 1, 1))
	}
	prev := reading(t0, p)

	s, _ := Sample(&prev, reading(t0.Add(5*time.Second), c))
	if len(s) > stats.MaxSeries {
		t.Errorf("%d series, more than %d", len(s), stats.MaxSeries)
	}
	if want := 7 + 3*libvirt.MaxStatsNICs + 4*libvirt.MaxStatsDisks; len(s) != want {
		t.Errorf("%d series, want %d: %v", len(s), want, s)
	}
	for k := range s {
		if len(k) > stats.MaxKeyLen {
			t.Errorf("key %q too long", k)
		}
	}
}

func TestVMSampleHealth(t *testing.T) {
	s, _ := Sample(nil, reading(t0, running(1, 1)))
	want(t, s, "health/ok", 1)

	st := running(1, 1)
	st.State = libvirt.StatePaused
	s, _ = Sample(nil, reading(t0, st))
	want(t, s, "health/ok", 0)

	h := HealthOnly()
	if len(h) != 1 || h["health/ok"] != 0 {
		t.Errorf("HealthOnly() = %v", h)
	}
	if _, ok := h["health/ok"]; !ok {
		t.Error("health/ok absent")
	}
}

func TestContractState(t *testing.T) {
	for in, out := range map[string]string{
		// libvirt's names
		"running": "running", "blocked": "running", "paused": "paused", "shutdown": "running",
		"shut-off": "stopped", "crashed": "crashed", "pmsuspended": "paused", "no-state": "unknown",
		"": "unknown", "garbage": "unknown",
		// as libvirt.StateName spells them
		// (running, paused, shut-off, crashed and unknown are spelt the same)
		libvirt.StateShuttingDown: "running", libvirt.StateSuspended: "paused", libvirt.StateCreating: "unknown",
		"Running": "unknown", "running\n": "unknown",
	} {
		if got := ContractState(in); got != out {
			t.Errorf("ContractState(%q) = %q, want %q", in, got, out)
		}
	}
}
