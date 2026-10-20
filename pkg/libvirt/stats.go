package libvirt

import (
	"errors"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
)

// AllStatsGroups asks ConnectGetAllDomainStats for state, CPU, balloon, vCPU,
// interface and block statistics.
const AllStatsGroups uint32 = 63

// A VM's series are permanent (48 at most), 7 of them fixed: 3 per NIC and 4
// per disk leave room for this many devices. The others are not reported.
const (
	MaxStatsNICs  = 4
	MaxStatsDisks = 7
)

const (
	maxNameLen       = 200 // bytes of a VM name
	maxDeviceNameLen = 32  // bytes of a disk name, which ends up in a series key
	maxDeviceIndex   = 4096
)

// DomainStats is one reading of a domain. A nil pointer: the domain did not
// report it.
type DomainStats struct {
	UUID, Name, State string // State as StateName spells it
	CPUTimeNs         *uint64
	VCPUs             *int

	MemAssignedKiB, MemAvailableKiB, MemUnusedKiB, MemUsableKiB *uint64

	NICs  []NICStats  // by Index
	Disks []DiskStats // by Name

	dom golibvirt.Domain
}

// NICStats are a NIC's counters. Index is its position in the domain XML.
type NICStats struct {
	Index                                            int
	MAC                                              string // canonical; empty when unknown
	RxBytes, RxPkts, RxDrop, TxBytes, TxPkts, TxDrop uint64

	ifname string // the host's tap device, a new one at every start
}

// ID names the NIC in series keys: its MAC, which survives restarts, else
// its index.
func (n NICStats) ID() string {
	if n.MAC != "" {
		return n.MAC
	}
	return "nic" + strconv.Itoa(n.Index)
}

// DiskStats are a disk's counters. Name is its target: vda, sda, …
type DiskStats struct {
	Name                             string
	RdBytes, RdReqs, WrBytes, WrReqs uint64
}

// StatsAPI is what reading statistics calls; *golibvirt.Libvirt has it.
type StatsAPI interface {
	ConnectGetAllDomainStats(doms []golibvirt.Domain, stats, flags uint32) ([]golibvirt.DomainStatsRecord, error)
	ConnectListAllDomains(needResults int32, flags golibvirt.ConnectListAllDomainsFlags) ([]golibvirt.Domain, uint32, error)
	DomainGetInfo(dom golibvirt.Domain) (state uint8, maxMem, memory uint64, nrVirtCPU uint16, cpuTime uint64, err error)
	DomainMemoryStats(dom golibvirt.Domain, maxStats, flags uint32) ([]golibvirt.DomainMemoryStat, error)
	DomainGetXMLDesc(dom golibvirt.Domain, flags golibvirt.DomainXMLFlags) (string, error)
	DomainBlockStats(dom golibvirt.Domain, path string) (rdReq, rdBytes, wrReq, wrBytes, errs int64, err error)
	DomainInterfaceStats(dom golibvirt.Domain, device string) (rxBytes, rxPkts, rxErrs, rxDrop, txBytes, txPkts, txErrs, txDrop int64, err error)
}

// ParseDomainStats reads one record of ConnectGetAllDomainStats. A device
// without byte counters (an empty CD-ROM drive) is left out; its other
// missing counters read 0.
func ParseDomainStats(rec golibvirt.DomainStatsRecord) DomainStats {
	s := DomainStats{UUID: uuid.UUID(rec.Dom.UUID).String(), Name: cleanText(rec.Dom.Name, maxNameLen), State: StateUnknown, dom: rec.Dom}

	nics := map[int]*NICStats{}
	disks := map[int]*DiskStats{}
	hasBytes := map[string]bool{} // "net.N" / "block.N"

	for _, p := range rec.Params {
		v, isUint := uintOf(p.Value)

		switch p.Field {
		case golibvirt.DomainStatsStateState:
			if isUint && v <= math.MaxInt32 {
				s.State = StateName(golibvirt.DomainState(v))
			}
			continue
		case golibvirt.DomainStatsCPUTime:
			s.CPUTimeNs = ptrIf(v, isUint)
			continue
		case golibvirt.DomainStatsVCPUCurrent:
			if isUint && v <= math.MaxInt32 {
				n := int(v)
				s.VCPUs = &n
			}
			continue
		case golibvirt.DomainStatsBalloonCurrent:
			s.MemAssignedKiB = ptrIf(v, isUint)
			continue
		case golibvirt.DomainStatsBalloonAvailable:
			s.MemAvailableKiB = ptrIf(v, isUint)
			continue
		case golibvirt.DomainStatsBalloonUnused:
			s.MemUnusedKiB = ptrIf(v, isUint)
			continue
		case golibvirt.DomainStatsBalloonUsable:
			s.MemUsableKiB = ptrIf(v, isUint)
			continue
		}

		if i, suffix, ok := deviceKey(p.Field, golibvirt.DomainStatsNetPrefix); ok {
			n := nics[i]
			if n == nil {
				n = &NICStats{Index: i}
				nics[i] = n
			}
			if suffix == golibvirt.DomainStatsNetSuffixName {
				if name, ok := p.Value.I.(string); ok {
					n.ifname = name
				}
				continue
			}
			counter := map[string]*uint64{
				golibvirt.DomainStatsNetSuffixRxBytes: &n.RxBytes, golibvirt.DomainStatsNetSuffixRxPkts: &n.RxPkts, golibvirt.DomainStatsNetSuffixRxDrop: &n.RxDrop,
				golibvirt.DomainStatsNetSuffixTxBytes: &n.TxBytes, golibvirt.DomainStatsNetSuffixTxPkts: &n.TxPkts, golibvirt.DomainStatsNetSuffixTxDrop: &n.TxDrop,
			}[suffix]
			if counter != nil && isUint {
				*counter = v
				if suffix == golibvirt.DomainStatsNetSuffixRxBytes || suffix == golibvirt.DomainStatsNetSuffixTxBytes {
					hasBytes["net."+strconv.Itoa(i)] = true
				}
			}
			continue
		}

		if i, suffix, ok := deviceKey(p.Field, golibvirt.DomainStatsBlockPrefix); ok {
			d := disks[i]
			if d == nil {
				d = &DiskStats{}
				disks[i] = d
			}
			if suffix == golibvirt.DomainStatsBlockSuffixName {
				if name, ok := p.Value.I.(string); ok {
					d.Name = cleanDeviceName(name)
				}
				continue
			}
			counter := map[string]*uint64{
				golibvirt.DomainStatsBlockSuffixRdBytes: &d.RdBytes, golibvirt.DomainStatsBlockSuffixRdReqs: &d.RdReqs,
				golibvirt.DomainStatsBlockSuffixWrBytes: &d.WrBytes, golibvirt.DomainStatsBlockSuffixWrReqs: &d.WrReqs,
			}[suffix]
			if counter != nil && isUint {
				*counter = v
				if suffix == golibvirt.DomainStatsBlockSuffixRdBytes || suffix == golibvirt.DomainStatsBlockSuffixWrBytes {
					hasBytes["block."+strconv.Itoa(i)] = true
				}
			}
		}
	}

	for _, i := range sortedKeys(nics) {
		if hasBytes["net."+strconv.Itoa(i)] {
			s.NICs = append(s.NICs, *nics[i])
		}
	}
	for _, i := range sortedKeys(disks) {
		if hasBytes["block."+strconv.Itoa(i)] && disks[i].Name != "" {
			s.Disks = append(s.Disks, *disks[i])
		}
	}
	capDevices(&s)

	return s
}

// AllDomainStats reads every domain with one call. Shut-off domains come back
// with their state only. NICs have no MAC yet: StatsReader adds it.
func AllDomainStats(l StatsAPI) ([]DomainStats, error) {
	recs, err := l.ConnectGetAllDomainStats(nil, AllStatsGroups, 0)
	if err != nil {
		return nil, err
	}

	out := make([]DomainStats, 0, len(recs))
	for _, rec := range recs {
		out = append(out, ParseDomainStats(rec))
	}
	return out, nil
}

// InfoDomainStats reads every domain with calls of its own, for drivers
// without ConnectGetAllDomainStats. A domain that vanishes while it is read
// is left out; a statistic the driver refuses is absent. An error that is not
// libvirt's (the connection) fails the whole reading.
func InfoDomainStats(l StatsAPI) ([]DomainStats, error) {
	doms, _, err := l.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, err
	}

	out := make([]DomainStats, 0, len(doms))
	for _, dom := range doms {
		s, err := infoDomainStats(l, dom)
		switch {
		case err == nil:
			out = append(out, s)
		case !isLibvirtError(err):
			return nil, err
		}
	}
	return out, nil
}

// infoDomainStats returns a libvirt error only when dom is gone.
func infoDomainStats(l StatsAPI, dom golibvirt.Domain) (DomainStats, error) {
	state, _, memory, vcpus, cpuTime, err := l.DomainGetInfo(dom)
	if err != nil {
		return DomainStats{}, err
	}

	s := DomainStats{UUID: uuid.UUID(dom.UUID).String(), Name: cleanText(dom.Name, maxNameLen), State: StateName(golibvirt.DomainState(state)), dom: dom}
	switch golibvirt.DomainState(state) {
	case golibvirt.DomainRunning, golibvirt.DomainBlocked, golibvirt.DomainPaused, golibvirt.DomainShutdown, golibvirt.DomainPmsuspended:
	default:
		return s, nil
	}

	n := int(vcpus)
	s.CPUTimeNs, s.VCPUs, s.MemAssignedKiB = &cpuTime, &n, &memory

	// optional reads: a libvirt error leaves them out, unless dom is gone
	optional := func(err error) (gone bool, fatal error) {
		if err == nil {
			return false, nil
		}
		if !isLibvirtError(err) || IsNotFound(err) {
			return true, err
		}
		return false, nil
	}

	mem, err := l.DomainMemoryStats(dom, uint32(golibvirt.DomainMemoryStatNr), 0)
	if stop, err := optional(err); stop {
		return DomainStats{}, err
	}
	for _, m := range mem {
		switch golibvirt.DomainMemoryStatTags(m.Tag) {
		case golibvirt.DomainMemoryStatAvailable:
			s.MemAvailableKiB = &m.Val
		case golibvirt.DomainMemoryStatUnused:
			s.MemUnusedKiB = &m.Val
		case golibvirt.DomainMemoryStatUsable:
			s.MemUsableKiB = &m.Val
		}
	}

	desc, err := l.DomainGetXMLDesc(dom, 0)
	if stop, err := optional(err); stop {
		return DomainStats{}, err
	}
	if err != nil {
		return s, nil
	}
	d, err := ParseDomainXML(desc)
	if err != nil {
		return s, nil
	}

	for _, target := range d.Targets {
		rdReq, rdBytes, wrReq, wrBytes, _, err := l.DomainBlockStats(dom, target)
		if stop, err := optional(err); stop {
			return DomainStats{}, err
		}
		name := cleanDeviceName(target)
		if err != nil || name == "" || (rdBytes < 0 && wrBytes < 0) {
			continue
		}
		s.Disks = append(s.Disks, DiskStats{Name: name, RdBytes: pos(rdBytes), RdReqs: pos(rdReq), WrBytes: pos(wrBytes), WrReqs: pos(wrReq)})
	}

	for i, nic := range d.NICs {
		mac := canonicalMAC(nic.MAC)
		if mac == "" {
			continue // libvirt finds a NIC by its MAC
		}
		rxB, rxP, _, rxD, txB, txP, _, txD, err := l.DomainInterfaceStats(dom, nic.MAC)
		if stop, err := optional(err); stop {
			return DomainStats{}, err
		}
		if err != nil || (rxB < 0 && txB < 0) {
			continue
		}
		s.NICs = append(s.NICs, NICStats{Index: i, MAC: mac, RxBytes: pos(rxB), RxPkts: pos(rxP), RxDrop: pos(rxD), TxBytes: pos(txB), TxPkts: pos(txP), TxDrop: pos(txD)})
	}

	capDevices(&s)

	return s, nil
}

// StatsReader reads all domains, with one call when the driver has
// ConnectGetAllDomainStats and with calls per domain when it does not, which
// it finds out once per connection.
type StatsReader struct {
	// OnFallback is told, once per connection, why the calls per domain are used.
	OnFallback func(error)

	mu       sync.Mutex
	conn     StatsAPI
	fallback bool
	macs     map[string]nicMACs // by UUID
}

// nicMACs are a domain's NIC MACs by index, read from its XML while its NICs
// had the host devices in key.
type nicMACs struct {
	key  string
	macs []string
}

// Read reads every domain on l.
func (r *StatsReader) Read(l StatsAPI) ([]DomainStats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.conn != l {
		r.conn, r.fallback, r.macs = l, false, nil
	}

	if !r.fallback {
		out, err := AllDomainStats(l)
		if err == nil {
			r.addMACs(l, out)
			return out, nil
		}
		if !needsFallback(err) {
			return nil, err
		}
		r.fallback = true
		if r.OnFallback != nil {
			r.OnFallback(err)
		}
	}

	return InfoDomainStats(l)
}

// addMACs gives the NICs their MAC. libvirt numbers a domain's net.N in the
// order of its XML; the XML is read again when the host devices change (a
// restart, a NIC plugged in). A NIC whose domain XML cannot be read is left
// out of this reading rather than reported under another name.
func (r *StatsReader) addMACs(l StatsAPI, all []DomainStats) {
	seen := map[string]nicMACs{}

	for k := range all {
		s := &all[k]
		if len(s.NICs) == 0 {
			continue
		}

		var key strings.Builder
		for _, n := range s.NICs {
			key.WriteString(strconv.Itoa(n.Index) + "=" + n.ifname + "\x00")
		}

		cached, ok := r.macs[s.UUID]
		if !ok || cached.key != key.String() {
			cached, ok = readMACs(l, s.dom, key.String())
		}
		if !ok {
			s.NICs = nil
			continue
		}
		seen[s.UUID] = cached

		for i := range s.NICs {
			if idx := s.NICs[i].Index; idx < len(cached.macs) {
				s.NICs[i].MAC = cached.macs[idx]
			}
		}
	}

	r.macs = seen
}

func readMACs(l StatsAPI, dom golibvirt.Domain, key string) (nicMACs, bool) {
	desc, err := l.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return nicMACs{}, false
	}
	d, err := ParseDomainXML(desc)
	if err != nil {
		return nicMACs{}, false
	}

	m := nicMACs{key: key}
	for _, nic := range d.NICs {
		m.macs = append(m.macs, canonicalMAC(nic.MAC))
	}
	return m, true
}

// IsNoSupport reports whether libvirt does not implement the call.
func IsNoSupport(err error) bool {
	var lerr golibvirt.Error
	if !errors.As(err, &lerr) {
		return false
	}

	switch golibvirt.ErrorNumber(lerr.Code) {
	case golibvirt.ErrNoSupport, golibvirt.ErrOperationUnsupported:
		return true
	}
	return false
}

// needsFallback: the driver lacks ConnectGetAllDomainStats, refuses it on a
// read-only connection, or refuses the groups asked.
func needsFallback(err error) bool {
	var lerr golibvirt.Error
	if !errors.As(err, &lerr) {
		return false
	}

	switch golibvirt.ErrorNumber(lerr.Code) {
	case golibvirt.ErrNoSupport, golibvirt.ErrOperationUnsupported, golibvirt.ErrOperationDenied, golibvirt.ErrArgumentUnsupported:
		return true
	}
	return false
}

func isLibvirtError(err error) bool {
	var lerr golibvirt.Error
	return errors.As(err, &lerr)
}

// uintOf reads a non-negative integer of any width.
func uintOf(v golibvirt.TypedParamValue) (uint64, bool) {
	switch x := v.I.(type) {
	case int32:
		return uint64(x), x >= 0
	case uint32:
		return uint64(x), true
	case int64:
		return uint64(x), x >= 0
	case uint64:
		return x, true
	}
	return 0, false
}

func ptrIf(v uint64, ok bool) *uint64 {
	if !ok {
		return nil
	}
	return &v
}

func pos(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// deviceKey splits "net.3.rx.bytes" into 3 and ".rx.bytes".
func deviceKey(field, prefix string) (int, string, bool) {
	rest, ok := strings.CutPrefix(field, prefix)
	if !ok {
		return 0, "", false
	}
	dot := strings.IndexByte(rest, '.')
	if dot < 1 {
		return 0, "", false
	}
	i, err := strconv.Atoi(rest[:dot])
	if err != nil || i < 0 || i > maxDeviceIndex {
		return 0, "", false
	}
	return i, rest[dot:], true
}

func sortedKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// capDevices keeps the first NICs by index and the first disks by name,
// one disk per name, so a VM's series stay the same from one reading to the
// next.
func capDevices(s *DomainStats) {
	slices.SortStableFunc(s.Disks, func(a, b DiskStats) int { return strings.Compare(a.Name, b.Name) })
	s.Disks = slices.CompactFunc(s.Disks, func(a, b DiskStats) bool { return a.Name == b.Name })
	s.Disks = s.Disks[:min(len(s.Disks), MaxStatsDisks)]
	s.NICs = s.NICs[:min(len(s.NICs), MaxStatsNICs)]
	if len(s.Disks) == 0 {
		s.Disks = nil
	}
	if len(s.NICs) == 0 {
		s.NICs = nil
	}
}

// cleanText is s without control characters or invalid UTF-8, at most max
// bytes.
func cleanText(s string, max int) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "") {
		if unicode.IsControl(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// cleanDeviceName keeps the characters a device target has (vda, sdb, hdc,
// nvme0n1), at most maxDeviceNameLen: it ends up in a series key.
func cleanDeviceName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() == maxDeviceNameLen {
			break
		}
		if r < utf8.RuneSelf && (r == '.' || r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// canonicalMAC is mac in lower case, or empty when it is not a MAC.
func canonicalMAC(mac string) string {
	hw, err := net.ParseMAC(mac)
	if err != nil || len(hw) != 6 {
		return ""
	}
	return hw.String()
}
