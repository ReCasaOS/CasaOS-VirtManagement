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

// StatsNoWait lets ConnectGetAllDomainStats skip the statistics of a domain
// whose job is busy (a migration, a snapshot) instead of waiting for it, which
// would hold up every VM's reading.
const StatsNoWait = uint32(golibvirt.ConnectGetAllDomainsStatsNowait)

// A VM's series are permanent (48 at most), 7 of them fixed: 3 per NIC and 4
// per disk leave room for this many devices. The cap holds per reading only: a
// VM whose devices change over its life can still reach 48, where Common's
// ErrSeriesCap is the backstop.
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

// NICStats are a NIC's counters, nil when not reported. Index is its position
// in the domain XML.
type NICStats struct {
	Index                                            int
	MAC                                              string // canonical; empty when unknown
	RxBytes, RxPkts, RxDrop, TxBytes, TxPkts, TxDrop *uint64

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

func (n NICStats) reported() bool {
	return n.RxBytes != nil || n.RxPkts != nil || n.RxDrop != nil || n.TxBytes != nil || n.TxPkts != nil || n.TxDrop != nil
}

// DiskStats are a disk's counters, nil when not reported. Name is its
// target: vda, sda, …
type DiskStats struct {
	Name                             string
	RdBytes, RdReqs, WrBytes, WrReqs *uint64
}

func (d DiskStats) reported() bool {
	return d.RdBytes != nil || d.RdReqs != nil || d.WrBytes != nil || d.WrReqs != nil
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
// without any counter (an empty CD-ROM drive, any disk of a shut-off domain)
// is left out.
func ParseDomainStats(rec golibvirt.DomainStatsRecord) DomainStats {
	s := DomainStats{UUID: uuid.UUID(rec.Dom.UUID).String(), Name: cleanText(rec.Dom.Name, maxNameLen), State: StateUnknown, dom: rec.Dom}

	nics := map[int]*NICStats{}
	disks := map[int]*DiskStats{}

	for _, p := range rec.Params {
		v, isUint := uintOf(p.Value)
		counter := ptrIf(v, isUint)
		name, isString := p.Value.I.(string)

		switch p.Field {
		case golibvirt.DomainStatsStateState:
			if isUint && v <= math.MaxInt32 {
				s.State = StateName(golibvirt.DomainState(v))
			}
			continue
		case golibvirt.DomainStatsCPUTime:
			s.CPUTimeNs = counter
			continue
		case golibvirt.DomainStatsVCPUCurrent:
			if isUint && v <= math.MaxInt32 {
				n := int(v)
				s.VCPUs = &n
			}
			continue
		case golibvirt.DomainStatsBalloonCurrent:
			s.MemAssignedKiB = counter
			continue
		case golibvirt.DomainStatsBalloonAvailable:
			s.MemAvailableKiB = counter
			continue
		case golibvirt.DomainStatsBalloonUnused:
			s.MemUnusedKiB = counter
			continue
		case golibvirt.DomainStatsBalloonUsable:
			s.MemUsableKiB = counter
			continue
		}

		if i, suffix, ok := deviceKey(p.Field, golibvirt.DomainStatsNetPrefix); ok {
			n := nics[i]
			if n == nil {
				n = &NICStats{Index: i}
				nics[i] = n
			}
			switch suffix {
			case golibvirt.DomainStatsNetSuffixName:
				if isString {
					n.ifname = name
				}
			case golibvirt.DomainStatsNetSuffixRxBytes:
				n.RxBytes = counter
			case golibvirt.DomainStatsNetSuffixRxPkts:
				n.RxPkts = counter
			case golibvirt.DomainStatsNetSuffixRxDrop:
				n.RxDrop = counter
			case golibvirt.DomainStatsNetSuffixTxBytes:
				n.TxBytes = counter
			case golibvirt.DomainStatsNetSuffixTxPkts:
				n.TxPkts = counter
			case golibvirt.DomainStatsNetSuffixTxDrop:
				n.TxDrop = counter
			}
			continue
		}

		if i, suffix, ok := deviceKey(p.Field, golibvirt.DomainStatsBlockPrefix); ok {
			d := disks[i]
			if d == nil {
				d = &DiskStats{}
				disks[i] = d
			}
			switch suffix {
			case golibvirt.DomainStatsBlockSuffixName:
				if isString {
					d.Name = cleanDeviceName(name)
				}
			case golibvirt.DomainStatsBlockSuffixRdBytes:
				d.RdBytes = counter
			case golibvirt.DomainStatsBlockSuffixRdReqs:
				d.RdReqs = counter
			case golibvirt.DomainStatsBlockSuffixWrBytes:
				d.WrBytes = counter
			case golibvirt.DomainStatsBlockSuffixWrReqs:
				d.WrReqs = counter
			}
		}
	}

	for _, i := range sortedKeys(nics) {
		if nics[i].reported() {
			s.NICs = append(s.NICs, *nics[i])
		}
	}
	for _, i := range sortedKeys(disks) {
		if disks[i].reported() && disks[i].Name != "" {
			s.Disks = append(s.Disks, *disks[i])
		}
	}
	capDevices(&s)

	return s
}

// AllDomainStats reads every domain with one call. A shut-off domain has its
// state, balloon and vCPU gauges but no CPU time and no device counters. NICs
// have no MAC yet: StatsReader adds it.
func AllDomainStats(l StatsAPI, flags uint32) ([]DomainStats, error) {
	recs, err := l.ConnectGetAllDomainStats(nil, AllStatsGroups, flags)
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
// without ConnectGetAllDomainStats, in the shape AllDomainStats gives. A
// domain that vanishes while it is read is left out, and a statistic the
// driver refuses for a domain (no balloon, empty CD-ROM) is absent. Any other
// error fails the whole reading, for every domain, as the single call would:
// a VM is never missing from a reading because of an error.
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
		case !IsNotFound(err):
			return nil, err
		}
	}
	return out, nil
}

// infoDomainStats reads dom; an error that IsNotFound means dom is gone.
func infoDomainStats(l StatsAPI, dom golibvirt.Domain) (DomainStats, error) {
	state, _, memory, vcpus, cpuTime, err := l.DomainGetInfo(dom)
	if err != nil {
		return DomainStats{}, err
	}

	n := int(vcpus)
	s := DomainStats{
		UUID:           uuid.UUID(dom.UUID).String(),
		Name:           cleanText(dom.Name, maxNameLen),
		State:          StateName(golibvirt.DomainState(state)),
		VCPUs:          &n,
		MemAssignedKiB: &memory,
		dom:            dom,
	}
	switch golibvirt.DomainState(state) {
	case golibvirt.DomainRunning, golibvirt.DomainBlocked, golibvirt.DomainPaused, golibvirt.DomainShutdown, golibvirt.DomainPmsuspended:
	default:
		return s, nil
	}
	s.CPUTimeNs = &cpuTime

	// a libvirt error on these leaves the statistic out, unless dom is gone
	stop := func(err error) bool { return err != nil && (!isLibvirtError(err) || IsNotFound(err)) }

	mem, err := l.DomainMemoryStats(dom, uint32(golibvirt.DomainMemoryStatNr), 0)
	if stop(err) {
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
	if stop(err) {
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
		if stop(err) {
			return DomainStats{}, err
		}
		disk := DiskStats{Name: cleanDeviceName(target), RdBytes: opt(rdBytes), RdReqs: opt(rdReq), WrBytes: opt(wrBytes), WrReqs: opt(wrReq)}
		if err == nil && disk.Name != "" && disk.reported() {
			s.Disks = append(s.Disks, disk)
		}
	}

	for i, x := range d.NICs {
		mac := canonicalMAC(x.MAC)
		if mac == "" {
			continue // libvirt finds a NIC by its MAC
		}
		rxB, rxP, _, rxD, txB, txP, _, txD, err := l.DomainInterfaceStats(dom, x.MAC)
		if stop(err) {
			return DomainStats{}, err
		}
		nic := NICStats{Index: i, MAC: mac, RxBytes: opt(rxB), RxPkts: opt(rxP), RxDrop: opt(rxD), TxBytes: opt(txB), TxPkts: opt(txP), TxDrop: opt(txD)}
		if err == nil && nic.reported() {
			s.NICs = append(s.NICs, nic)
		}
	}

	capDevices(&s)

	return s, nil
}

// StatsReader reads all domains, with one call when the driver has
// ConnectGetAllDomainStats and with calls per domain when it does not, which
// it finds out once per connection. Call Reset on every new connection
// (Conn.OnConnect).
type StatsReader struct {
	// OnFallback is told, once per connection, why the calls per domain are used.
	OnFallback func(error)
	// OnNoWaitRefused is told, once per connection, why StatsNoWait is no
	// longer asked.
	OnNoWaitRefused func(error)

	mu       sync.Mutex
	fallback bool
	noWait   bool               // StatsNoWait refused: an old libvirt or the test driver
	macs     map[string]nicMACs // by UUID
}

// nicMACs are a domain's NIC MACs by index, read from its XML while its NICs
// had the host devices in key.
type nicMACs struct {
	key  string
	macs []string
}

// Reset forgets what was learnt of the previous connection.
func (r *StatsReader) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.fallback, r.noWait, r.macs = false, false, nil
}

// Read reads every domain on l. The callbacks are called once the reader's
// lock is released, so they may call Reset.
func (r *StatsReader) Read(l StatsAPI) ([]DomainStats, error) {
	out, tell, err := r.read(l)
	if tell != nil {
		tell()
	}
	return out, err
}

func (r *StatsReader) read(l StatsAPI) (_ []DomainStats, tell func(), _ error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.fallback {
		out, refused, err := r.readAll(l)
		if refused != nil && r.OnNoWaitRefused != nil {
			tell = func() { r.OnNoWaitRefused(refused) }
		}
		if err == nil {
			r.addMACs(l, out)
			return out, tell, nil
		}
		if !needsFallback(err) {
			return nil, tell, err
		}
		r.fallback = true
		if r.OnFallback != nil {
			tell = func() { r.OnFallback(err) }
		}
	}

	out, err := InfoDomainStats(l)
	return out, tell, err
}

// readAll also returns the refusal of StatsNoWait when it was just learnt.
func (r *StatsReader) readAll(l StatsAPI) (_ []DomainStats, refused, _ error) {
	if r.noWait {
		out, err := AllDomainStats(l, 0)
		return out, nil, err
	}
	out, err := AllDomainStats(l, StatsNoWait)
	if !refusesFlag(err) {
		return out, nil, err
	}
	out, retryErr := AllDomainStats(l, 0)
	if retryErr != nil {
		return nil, nil, retryErr
	}
	r.noWait = true
	return out, err, nil
}

// refusesFlag: how libvirt (old ones, other drivers) refuses a flag it does
// not know.
func refusesFlag(err error) bool {
	var lerr golibvirt.Error
	if !errors.As(err, &lerr) {
		return false
	}

	switch golibvirt.ErrorNumber(lerr.Code) {
	case golibvirt.ErrInvalidArg, golibvirt.ErrNoSupport, golibvirt.ErrOperationInvalid, golibvirt.ErrArgumentUnsupported:
		return true
	}
	return false
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
			key.WriteString(strconv.Itoa(n.Index))
			key.WriteByte('=')
			key.WriteString(n.ifname)
			key.WriteByte(0)
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

// opt is a counter of the per-domain calls, where -1 means not reported.
func opt(v int64) *uint64 {
	return ptrIf(uint64(v), v >= 0)
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
