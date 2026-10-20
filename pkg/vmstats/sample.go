// Package vmstats turns two libvirt readings of a VM into a stats.Sample.
// Pure: no libvirt calls, no clock.
package vmstats

import (
	"time"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
)

const (
	Interval = 5 * time.Second
	// MaxGap is the longest interval a rate is computed over; beyond it
	// (libvirtd away, a missed tick) the interval is a gap, not a spike.
	MaxGap = 3 * Interval
)

// Reading is one libvirt reading of a VM and when it was taken.
type Reading struct {
	At time.Time
	S  libvirt.DomainStats
}

// ContractState maps a libvirt state, as libvirt names it or as
// libvirt.StateName spells it, to the contract's state.
func ContractState(libvirtState string) string {
	switch libvirtState {
	case "running", "blocked", "shutdown", libvirt.StateShuttingDown:
		return "running"
	case "paused", "pmsuspended", libvirt.StateSuspended:
		return "paused"
	case "shut-off":
		return "stopped"
	case "crashed":
		return "crashed"
	}
	return "unknown"
}

// HealthOnly is the whole sample of a recorded VM that is no longer running
// or paused.
func HealthOnly() stats.Sample { return stats.Sample{"health/ok": 0} }

// Sample computes cur's gauges, and its rates against prev when prev is close
// enough before it. A value without data is absent, never 0: a counter must
// be reported in both readings and must not have gone back (a restart). It
// also says whether memory/used could not be had (no balloon driver in the
// guest), for the caller's note.
func Sample(prev *Reading, cur Reading) (s stats.Sample, memUsedUnavailable bool) {
	s = stats.Sample{"health/ok": 0}
	c := cur.S
	if ContractState(c.State) == "running" {
		s["health/ok"] = 1
	}

	if c.VCPUs != nil {
		s["cpu/vcpus"] = float64(*c.VCPUs)
	}
	if c.MemAssignedKiB != nil {
		s["memory/assigned"] = kib(*c.MemAssignedKiB)
	}
	used, hasUsed := memUsedKiB(c)
	if hasUsed {
		s["memory/used"] = kib(used)
		if c.MemAssignedKiB != nil {
			s["alert/mem_high"] = float64(used) / float64(*c.MemAssignedKiB) * 100
		}
	}

	if prev == nil {
		return s, !hasUsed
	}
	gap := cur.At.Sub(prev.At)
	if gap <= 0 || gap > MaxGap {
		return s, !hasUsed
	}
	dt := gap.Seconds()
	p := prev.S
	rate := func(key string, a, b *uint64) {
		if d, ok := delta(a, b); ok {
			s[key] = float64(d) / dt
		}
	}

	if d, ok := delta(p.CPUTimeNs, c.CPUTimeNs); ok {
		total := float64(d) / (dt * 1e9) * 100
		s["cpu/total"] = total
		if c.VCPUs != nil && *c.VCPUs > 0 {
			s["alert/cpu_high"] = total / float64(*c.VCPUs)
		}
	}

	for _, n := range c.NICs[:min(len(c.NICs), libvirt.MaxStatsNICs)] {
		id := n.ID()
		o, ok := findNIC(p.NICs, id)
		if !ok {
			continue
		}
		rate("network/rx:"+id, o.RxBytes, n.RxBytes)
		rate("network/tx:"+id, o.TxBytes, n.TxBytes)
		rx, okRx := delta(o.RxDrop, n.RxDrop)
		tx, okTx := delta(o.TxDrop, n.TxDrop)
		if okRx && okTx {
			s["network/dropped:"+id] = (float64(rx) + float64(tx)) / dt
		}
	}

	for _, d := range c.Disks[:min(len(c.Disks), libvirt.MaxStatsDisks)] {
		o, ok := findDisk(p.Disks, d.Name)
		if !ok {
			continue
		}
		rate("disk/read:"+d.Name, o.RdBytes, d.RdBytes)
		rate("disk/write:"+d.Name, o.WrBytes, d.WrBytes)
		rate("disk/iops_read:"+d.Name, o.RdReqs, d.RdReqs)
		rate("disk/iops_write:"+d.Name, o.WrReqs, d.WrReqs)
	}

	return s, !hasUsed
}

// memUsedKiB is the guest's memory in use: available minus usable, else
// minus unused. Nothing when the balloon does not report it (it reads 0 or
// only partly without a driver in the guest) or the result is not a plausible
// amount: zero, negative or more than assigned.
func memUsedKiB(c libvirt.DomainStats) (uint64, bool) {
	if c.MemAvailableKiB == nil {
		return 0, false
	}
	free := c.MemUsableKiB
	if free == nil {
		free = c.MemUnusedKiB
	}
	if free == nil || *free >= *c.MemAvailableKiB {
		return 0, false
	}
	used := *c.MemAvailableKiB - *free
	if c.MemAssignedKiB != nil && used > *c.MemAssignedKiB {
		return 0, false
	}
	return used, true
}

// delta is b-a when both are reported and the counter did not go back.
func delta(a, b *uint64) (uint64, bool) {
	if a == nil || b == nil || *b < *a {
		return 0, false
	}
	return *b - *a, true
}

// kib is KiB in bytes, as a float: it cannot overflow.
func kib(v uint64) float64 { return float64(v) * 1024 }

func findNIC(nics []libvirt.NICStats, id string) (libvirt.NICStats, bool) {
	for _, n := range nics {
		if n.ID() == id {
			return n, true
		}
	}
	return libvirt.NICStats{}, false
}

func findDisk(disks []libvirt.DiskStats, name string) (libvirt.DiskStats, bool) {
	for _, d := range disks {
		if d.Name == name {
			return d, true
		}
	}
	return libvirt.DiskStats{}, false
}
