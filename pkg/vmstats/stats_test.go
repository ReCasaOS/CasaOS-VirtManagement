package vmstats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	vmA = "11111111-1111-1111-1111-111111111111"
	vmB = "22222222-2222-2222-2222-222222222222"
	vmC = "33333333-3333-3333-3333-333333333333"
)

type fakeSource struct {
	mu         sync.Mutex
	doms       []libvirt.DomainStats
	err        error
	titles     map[string][2]string // by UUID: title, detail
	descErr    error
	descCalls  map[string]int
	descDelay  time.Duration
	onSnapshot func()
}

func (f *fakeSource) set(doms []libvirt.DomainStats, err error) {
	f.mu.Lock()
	f.doms, f.err = doms, err
	f.mu.Unlock()
}

func (f *fakeSource) Snapshot() ([]libvirt.DomainStats, error) {
	f.mu.Lock()
	hook, doms, err := f.onSnapshot, slices.Clone(f.doms), f.err
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return doms, err
}

func (f *fakeSource) Describe(id string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.descCalls[id]++
	time.Sleep(f.descDelay)
	if f.descErr != nil {
		return "", "", f.descErr
	}
	if t, ok := f.titles[id]; ok {
		return t[0], t[1], nil
	}
	return "", "", errors.New("no such domain")
}

func (f *fakeSource) calls(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.descCalls[id]
}

type published struct {
	name  string
	props map[string]string
}

type fakePublisher struct {
	mu      sync.Mutex
	got     []published
	flushed chan struct{}
	entered chan struct{} // told when Publish starts, if set
	block   chan struct{} // Publish waits on it, if set
}

func (p *fakePublisher) Publish(name string, props map[string]string) {
	if name == "flush" {
		p.flushed <- struct{}{}
		return
	}
	if p.entered != nil {
		p.entered <- struct{}{}
	}
	if p.block != nil {
		<-p.block
	}
	p.mu.Lock()
	p.got = append(p.got, published{name, props})
	p.mu.Unlock()
}

type rig struct {
	t     *testing.T
	dir   string
	clock *stats.FakeClock
	src   *fakeSource
	pub   *fakePublisher
	s     *Stats
	cpu   map[string]uint64

	mu   sync.Mutex
	logs []string
}

func newRig(t *testing.T) *rig {
	r := &rig{
		t: t, dir: t.TempDir(), clock: stats.NewFakeClock(base),
		src: &fakeSource{titles: map[string][2]string{}, descCalls: map[string]int{}},
		pub: &fakePublisher{flushed: make(chan struct{})},
		cpu: map[string]uint64{},
	}
	old := logError
	logError = func(msg string, _ error) {
		r.mu.Lock()
		r.logs = append(r.logs, msg)
		r.mu.Unlock()
	}
	t.Cleanup(func() { logError = old })
	r.open()
	return r
}

func (r *rig) open() {
	s, err := New(Options{Dir: r.dir, Clock: r.clock, Source: r.src, Publisher: r.pub})
	if err != nil {
		r.t.Fatal(err)
	}
	r.s = s
	r.t.Cleanup(func() { _ = s.Close() })
}

// logged counts the log lines that contain sub.
func (r *rig) logged(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// tick takes a snapshot of doms now, then moves the clock one interval on.
func (r *rig) tick(doms ...libvirt.DomainStats) {
	r.src.set(doms, nil)
	r.s.Tick()
	r.clock.Advance(Interval)
}

func (r *rig) fail(advance time.Duration) {
	r.src.set(nil, errors.New("libvirt is down"))
	r.s.Tick()
	r.clock.Advance(advance)
}

// vm is a running 1-vCPU VM that used pct % of a core since the last call.
func (r *rig) vm(id string, pct float64) libvirt.DomainStats {
	r.cpu[id] += uint64(pct / 100 * 5e9)
	return libvirt.DomainStats{UUID: id, Name: "vm-" + id[:4], State: libvirt.StateRunning, CPUTimeNs: u(r.cpu[id]), VCPUs: n(1), MemAssignedKiB: u(1048576)}
}

func stopped(id, state string) libvirt.DomainStats {
	return libvirt.DomainStats{UUID: id, Name: "vm-" + id[:4], State: state, VCPUs: n(1), MemAssignedKiB: u(1048576)}
}

// run ticks the VM at pct for the given number of minutes.
func (r *rig) run(id string, pct float64, minutes int) {
	for range minutes * 12 {
		r.tick(r.vm(id, pct))
	}
}

// events are the events published so far.
func (r *rig) events() []published {
	if r.s.pub.submit("flush", nil) {
		<-r.pub.flushed
	}
	r.pub.mu.Lock()
	defer r.pub.mu.Unlock()
	return slices.Clone(r.pub.got)
}

func (r *rig) count(name string) int {
	n := 0
	for _, e := range r.events() {
		if e.name == name {
			n++
		}
	}
	return n
}

func (r *rig) series(id string, rg stats.Range) Series {
	r.t.Helper()
	res, err := r.s.Series(id, rg)
	if err != nil {
		r.t.Fatalf("Series(%s, %s): %v", id, rg, err)
	}
	return res
}

func points(res Series, chart, name string) []stats.Point {
	for _, sd := range res.Series {
		if sd.Chart == chart && sd.Name == name {
			return sd.Points
		}
	}
	return nil
}

// at is the value of the point at t; false for a gap or no point.
func at(pts []stats.Point, t time.Time) (float64, bool) {
	for _, p := range pts {
		if p.T == t.Unix() && p.Avg != nil {
			return *p.Avg, true
		}
	}
	return 0, false
}

func lastValue(pts []stats.Point) (float64, bool) {
	for i := len(pts) - 1; i >= 0; i-- {
		if pts[i].Avg != nil {
			return *pts[i].Avg, true
		}
	}
	return 0, false
}

func (r *rig) item(id string) Item {
	r.t.Helper()
	items, err := r.s.Items()
	if err != nil {
		r.t.Fatalf("Items: %v", err)
	}
	for _, it := range items {
		if it.ID == id {
			return it
		}
	}
	r.t.Fatalf("%s not listed", id)
	return Item{}
}

func listed(s *Stats, id string) bool {
	items, _ := s.Items()
	return slices.ContainsFunc(items, func(it Item) bool { return it.ID == id })
}

func TestStatsTickRecordsRunningVM(t *testing.T) {
	r := newRig(t)
	d := r.vm(vmA, 0)
	r.tick(d)
	d.CPUTimeNs = u(*d.CPUTimeNs + 5e9)
	r.tick(d)

	if v, ok := lastValue(points(r.series("vm:"+vmA, stats.RangeLive), "cpu", "total")); !ok || v != 100 {
		t.Errorf("cpu/total last %v %v, want 100", v, ok)
	}
	it := r.item("vm:" + vmA)
	if it.State != "running" || it.Group != "Machines virtuelles" || it.Kind != "vm" || it.CPUPercent == nil || *it.CPUPercent != 100 {
		t.Errorf("item %+v", it)
	}
	if it.MemoryLimit == nil || *it.MemoryLimit != 1048576*1024 || it.MemoryUsed != nil || it.StartedAt != nil || it.Detail != nil {
		t.Errorf("item %+v", it)
	}
}

func TestStatsItemsSortedByName(t *testing.T) {
	r := newRig(t)
	a, b := r.vm(vmA, 0), r.vm(vmB, 0)
	a.Name, b.Name = "Beta", "alpha"
	r.tick(a, b)
	items, _ := r.s.Items()
	if len(items) != 2 || items[0].Name != "alpha" || items[1].Name != "Beta" {
		t.Errorf("%+v", items)
	}
}

func TestStatsStoppedVMKeepsListedWithoutSamples(t *testing.T) {
	r := newRig(t)
	r.tick(stopped(vmA, libvirt.StateShutOff))
	r.tick(stopped(vmA, libvirt.StateShutOff))

	it := r.item("vm:" + vmA)
	if it.State != "stopped" || it.CPUPercent != nil || it.MemoryUsed != nil {
		t.Errorf("item %+v", it)
	}
	if slices.Contains(r.s.store.Items(), "vm:"+vmA) {
		t.Error("a VM never seen running has history")
	}
	// listed: its series answers, empty
	if res := r.series("vm:"+vmA, stats.Range15m); len(res.Series) != 0 || res.StepSeconds != 60 {
		t.Errorf("%+v", res)
	}
}

func TestStatsStoppedVMRecordsHealthZero(t *testing.T) {
	r := newRig(t)
	r.run(vmA, 50, 1)
	stop := r.clock.Now()
	for range 3 {
		r.tick(stopped(vmA, libvirt.StateShutOff))
	}

	live := r.series("vm:"+vmA, stats.RangeLive)
	health := points(live, "health", "ok")
	if v, ok := at(health, stop.Add(-Interval)); !ok || v != 1 {
		t.Errorf("health/ok running %v %v", v, ok)
	}
	for i := range 3 {
		when := stop.Add(time.Duration(i) * Interval)
		if v, ok := at(health, when); !ok || v != 0 {
			t.Errorf("health/ok stopped at %d: %v %v, want a 0 point", i, v, ok)
		}
		if v, ok := at(points(live, "cpu", "total"), when); ok {
			t.Errorf("cpu/total stopped at %d: %v, want a gap", i, v)
		}
	}
}

// fire runs vmA at 100 % until cpu_high fires.
func (r *rig) fire() {
	r.t.Helper()
	r.src.titles[vmA] = [2]string{"Web", ""}
	r.run(vmA, 100, 11)
	if n := r.count(EventFiring); n != 1 {
		r.t.Fatalf("%d firing, want 1", n)
	}
}

func TestStatsStoppedVMResolvesItsFiringAlert(t *testing.T) {
	for _, state := range []string{libvirt.StateShutOff, libvirt.StateCrashed} {
		t.Run(state, func(t *testing.T) {
			r := newRig(t)
			r.fire()
			r.tick(stopped(vmA, state))
			if n := r.count(EventResolved); n != 1 {
				t.Fatalf("%d resolved at the first tick, want 1", n)
			}
			// a VM libvirt shows crashed also has vm_crashed (firing, kept while it stays so)
			crashed := 0
			if state == libvirt.StateCrashed {
				crashed = 1
			}
			if a := r.item("vm:" + vmA).Alerts; a != crashed {
				t.Errorf("alerts %d", a)
			}
			for range 30 {
				r.tick(stopped(vmA, state))
			}
			if n := len(r.events()); n != 2+crashed {
				t.Errorf("%d events, want %d: %+v", n, 2+crashed, r.events())
			}
		})
	}
}

func TestStatsDeletedVMResolvesItsFiringAlert(t *testing.T) {
	r := newRig(t)
	r.fire()
	r.tick()
	if n := r.count(EventResolved); n != 0 || !listed(r.s, "vm:"+vmA) {
		t.Fatalf("after one missed snapshot: %d resolved, listed %v", n, listed(r.s, "vm:"+vmA))
	}
	r.tick()
	if n := r.count(EventResolved); n != 1 || listed(r.s, "vm:"+vmA) {
		t.Fatalf("after two: %d resolved, listed %v", n, listed(r.s, "vm:"+vmA))
	}
	for range 3 {
		r.tick()
	}
	if n := len(r.events()); n != 2 {
		t.Errorf("%d events", n)
	}
	if res := r.series("vm:"+vmA, stats.Range15m); len(points(res, "cpu", "total")) == 0 {
		t.Error("the history of a deleted VM is gone")
	}
}

func TestStatsFailedSnapshotDoesNotResolve(t *testing.T) {
	r := newRig(t)
	r.fire()
	for range 5 { // three minutes
		r.fail(36 * time.Second)
	}
	r.run(vmA, 100, 2)
	if f, res := r.count(EventFiring), r.count(EventResolved); f != 1 || res != 0 {
		t.Fatalf("after the outage: %d firing, %d resolved", f, res)
	}
	r.run(vmA, 10, 2)
	if f, res := r.count(EventFiring), r.count(EventResolved); f != 1 || res != 1 {
		t.Errorf("low: %d firing, %d resolved", f, res)
	}
}

// The first reading after a gap has no CPU rate: if it is alone in its
// minute, that minute would end cpu_high; it is not recorded.
func TestStatsFirstReadingAfterGapAtMinuteEnd(t *testing.T) {
	r := newRig(t)
	r.fire() // the clock is now at 11:00
	for range 11 {
		r.fail(Interval)
	}
	if r.clock.Now().Second() != 55 {
		t.Fatalf("clock %v", r.clock.Now())
	}
	r.run(vmA, 100, 2)
	if f, res := r.count(EventFiring), r.count(EventResolved); f != 1 || res != 0 {
		t.Errorf("%d firing, %d resolved", f, res)
	}
}

func TestStatsStartedAtAndDetail(t *testing.T) {
	r := newRig(t)
	long := strings.Repeat("é", 300)
	r.src.titles[vmA] = [2]string{"Web", long}
	r.tick(r.vm(vmA, 10), stopped(vmB, libvirt.StateShutOff))

	a := r.item("vm:" + vmA)
	if a.StartedAt != nil {
		t.Errorf("already running at the start: started_at %v", a.StartedAt)
	}
	if a.Name != "Web" || a.Detail == nil || *a.Detail != strings.Repeat("é", 200) {
		t.Errorf("name %q detail %v", a.Name, a.Detail)
	}
	if b := r.item("vm:" + vmB); b.Name != "vm-2222" || b.Detail != nil {
		t.Errorf("failed Describe: %+v", b)
	}

	// not running, then running: started now
	startB := r.clock.Now()
	r.tick(r.vm(vmA, 10), r.vm(vmB, 10))
	if b := r.item("vm:" + vmB); b.StartedAt == nil || !b.StartedAt.Equal(startB) || b.State != "running" {
		t.Errorf("started_at %v, want %v", b.StartedAt, startB)
	}

	// a Started event gives the time
	when := r.clock.Now().Add(-2 * time.Second)
	r.s.noteStarted(vmA, when)
	r.tick(r.vm(vmA, 10), r.vm(vmB, 10))
	if a := r.item("vm:" + vmA); a.StartedAt == nil || !a.StartedAt.Equal(when) {
		t.Errorf("after Started: %v, want %v", a.StartedAt, when)
	}

	// a failed Describe keeps the previous name and detail
	r.src.descErr = errors.New("libvirt said no")
	for range 61 { // past the 5 min refresh
		r.tick(r.vm(vmA, 10), stopped(vmB, libvirt.StateShutOff))
	}
	if a := r.item("vm:" + vmA); a.Name != "Web" || a.Detail == nil || *a.Detail != strings.Repeat("é", 200) {
		t.Errorf("after a failed Describe: %q %v", a.Name, a.Detail)
	}
	if b := r.item("vm:" + vmB); b.StartedAt != nil {
		t.Errorf("stopped: started_at %v", b.StartedAt)
	}
}

func TestStatsCleansText(t *testing.T) {
	r := newRig(t)
	r.src.titles[vmA] = [2]string{"We‮b\x07 ​shop", "a\x1b[31mb⁦c"}
	d := r.vm(vmB, 0)
	d.Name = "‮db\x00"
	r.tick(r.vm(vmA, 0), d)
	if a := r.item("vm:" + vmA); a.Name != "Web shop" || a.Detail == nil || *a.Detail != "a[31mbc" {
		t.Errorf("%q %v", a.Name, *a.Detail)
	}
	if b := r.item("vm:" + vmB); b.Name != "db" {
		t.Errorf("%q", b.Name)
	}
	if got := cleanText(strings.Repeat("x", 250)); len(got) != maxText {
		t.Errorf("not bounded: %d", len(got))
	}
}

func TestStatsCapacityNote(t *testing.T) {
	if testing.Short() {
		t.Skip("creates 256 history files")
	}
	r := newRig(t)
	doms := make([]libvirt.DomainStats, stats.MaxItems+1)
	for i := range doms {
		// health/ok only: one series, the smallest ring
		doms[i] = libvirt.DomainStats{UUID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i), State: libvirt.StateRunning}
	}
	for range 3 {
		r.tick(doms...)
	}
	if n := r.logged("too many VMs"); n != 1 {
		t.Errorf("logged %d times, want 1", n)
	}
	if got := r.s.Notes(); !slices.Equal(got, []string{"stats_capacity"}) {
		t.Errorf("notes %v", got)
	}
	r.clock.Advance(50 * time.Minute)
	if got := r.s.Notes(); !slices.Equal(got, []string{"stats_capacity"}) {
		t.Errorf("50 min later: %v", got)
	}
	r.clock.Advance(15 * time.Minute)
	if got := r.s.Notes(); len(got) != 0 || got == nil {
		t.Errorf("an hour later: %#v", got)
	}
	r.tick(doms...)
	if n := r.logged("too many VMs"); n != 2 {
		t.Errorf("an hour later: logged %d times, want 2", n)
	}
}

func TestStatsLibvirtdOutage(t *testing.T) {
	r := newRig(t)
	for range 3 {
		r.tick(r.vm(vmA, 50))
	}
	r.fail(20 * time.Second)
	r.fail(20 * time.Second)
	if _, err := r.s.Items(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("list during the outage: %v, want ErrUnavailable", err)
	}
	r.fail(Interval) // 40 s of errors
	back := r.clock.Now()
	d := r.vm(vmA, 50)
	d.CPUTimeNs = u(*d.CPUTimeNs + 1000e9)
	r.tick(d)
	d.CPUTimeNs = u(*d.CPUTimeNs + 2.5e9)
	r.tick(d)

	if !listed(r.s, "vm:"+vmA) {
		t.Fatal("forgotten during the outage")
	}
	live := r.series("vm:"+vmA, stats.RangeLive)
	if v, ok := at(points(live, "cpu", "total"), back); ok {
		t.Errorf("cpu/total %v right after the outage, want a gap", v)
	}
	if _, ok := at(points(live, "health", "ok"), back); !ok {
		t.Error("the reading after the outage was not recorded")
	}
	if v, ok := at(points(live, "cpu", "total"), back.Add(Interval)); !ok || v != 50 {
		t.Errorf("cpu/total after: %v %v", v, ok)
	}
	for _, when := range []time.Time{back.Add(-45 * time.Second), back.Add(-25 * time.Second), back.Add(-5 * time.Second)} {
		if _, ok := at(points(live, "health", "ok"), when); ok {
			t.Errorf("a point at a failed snapshot (%v)", when)
		}
	}
	if n := r.logged("libvirt is unreachable"); n != 1 {
		t.Errorf("outage logged %d times", n)
	}
}

// The list is unavailable until a snapshot succeeds, and again from a failed
// one until the next success: libvirtd away is not an empty host.
func TestStatsListUnavailableWhileLibvirtIsDown(t *testing.T) {
	r := newRig(t)
	if _, err := r.s.Items(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("before any snapshot: %v", err)
	}
	r.fail(Interval)
	if _, err := r.s.Items(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("libvirt never reached: %v", err)
	}
	r.tick()
	if items, err := r.s.Items(); err != nil || len(items) != 0 {
		t.Errorf("no VM: %v %v", items, err)
	}
	r.tick(r.vm(vmA, 10))
	r.fail(Interval)
	if _, err := r.s.Items(); !errors.Is(err, ErrUnavailable) {
		t.Errorf("outage: %v", err)
	}
	r.tick(r.vm(vmA, 10))
	if items, err := r.s.Items(); err != nil || len(items) != 1 {
		t.Errorf("back: %v %v", items, err)
	}
}

func TestStatsForgetsAfterTwoSnapshotsWithoutTheVM(t *testing.T) {
	r := newRig(t)
	r.run(vmA, 10, 2)
	r.tick(r.vm(vmB, 10))
	if !listed(r.s, "vm:"+vmA) {
		t.Fatal("forgotten after one missed snapshot")
	}
	r.tick(r.vm(vmB, 10))
	if listed(r.s, "vm:"+vmA) {
		t.Fatal("still listed after two")
	}
	if res := r.series("vm:"+vmA, stats.Range15m); len(points(res, "cpu", "total")) == 0 {
		t.Error("history gone")
	}
	// back: one missed snapshot between two is not two in a row
	r.tick(r.vm(vmA, 10), r.vm(vmB, 10))
	r.tick(r.vm(vmA, 10))
	r.tick(r.vm(vmA, 10), r.vm(vmB, 10))
	r.tick(r.vm(vmA, 10))
	if !listed(r.s, "vm:"+vmB) {
		t.Error("a VM missing every other snapshot was forgotten")
	}
}

func TestStatsRenameKeepsHistory(t *testing.T) {
	r := newRig(t)
	r.src.titles[vmA] = [2]string{"Old", ""}
	r.run(vmA, 10, 2)
	if got := r.item("vm:" + vmA).Name; got != "Old" {
		t.Fatalf("name %q", got)
	}
	r.src.titles[vmA] = [2]string{"New", ""}
	r.run(vmA, 10, 4)
	if got := r.item("vm:" + vmA).Name; got != "New" {
		t.Errorf("after 6 min: %q", got)
	}
	if pts := points(r.series("vm:"+vmA, stats.Range15m), "cpu", "total"); func() int {
		n := 0
		for _, p := range pts {
			if p.Avg != nil {
				n++
			}
		}
		return n
	}() < 5 {
		t.Errorf("history lost: %v", pts)
	}
}

func TestStatsMemoryNote(t *testing.T) {
	r := newRig(t)
	balloon := r.vm(vmB, 10)
	balloon.MemAvailableKiB, balloon.MemUsableKiB = u(1048576), u(524288)
	r.tick(r.vm(vmA, 10), balloon)
	r.tick(r.vm(vmA, 10), balloon)

	a := r.series("vm:"+vmA, stats.RangeLive)
	if !slices.Equal(a.Notes, []string{"vm_memory_used_unavailable"}) || points(a, "memory", "used") != nil {
		t.Errorf("no balloon: notes %v", a.Notes)
	}
	b := r.series("vm:"+vmB, stats.RangeLive)
	if len(b.Notes) != 0 || points(b, "memory", "used") == nil {
		t.Errorf("balloon: notes %v", b.Notes)
	}
	if it := r.item("vm:" + vmB); it.MemoryUsed == nil || *it.MemoryUsed != 524288*1024 {
		t.Errorf("memory_used %v", it.MemoryUsed)
	}
}

func TestStatsSeriesHidesAlertKeys(t *testing.T) {
	r := newRig(t)
	balloon := r.vm(vmA, 100)
	balloon.MemAvailableKiB, balloon.MemUsableKiB = u(1048576), u(1)
	for range 14 {
		d := r.vm(vmA, 100)
		d.MemAvailableKiB, d.MemUsableKiB = balloon.MemAvailableKiB, balloon.MemUsableKiB
		r.tick(d)
	}
	for _, rg := range []stats.Range{stats.RangeLive, stats.Range15m} {
		res := r.series("vm:"+vmA, rg)
		if len(res.Series) == 0 {
			t.Fatalf("%s: no series", rg)
		}
		for _, sd := range res.Series {
			if sd.Chart == "alert" {
				t.Errorf("%s: %s/%s returned", rg, sd.Chart, sd.Name)
			}
		}
	}
}

func TestStatsUnknownItem(t *testing.T) {
	r := newRig(t)
	r.tick(r.vm(vmA, 10))
	for _, id := range []string{"vm:00000000-0000-0000-0000-000000000000", vmA, "app:" + vmA} {
		if _, err := r.s.Series(id, stats.RangeLive); !errors.Is(err, ErrUnknownItem) {
			t.Errorf("Series(%s): %v", id, err)
		}
		if _, _, err := r.s.Alerts(id); !errors.Is(err, ErrUnknownItem) {
			t.Errorf("Alerts(%s): %v", id, err)
		}
		if err := r.s.SetRules(id, stats.DefaultRules()); !errors.Is(err, ErrUnknownItem) {
			t.Errorf("SetRules(%s): %v", id, err)
		}
	}
	if _, err := r.s.Series("vm:"+vmA, "2d"); !errors.Is(err, stats.ErrBadRange) {
		t.Errorf("bad range: %v", err)
	}
}

func TestStatsCPUHighAlert(t *testing.T) {
	r := newRig(t)
	r.src.titles[vmA] = [2]string{"Web", ""}
	r.run(vmA, 100, 10)
	r.tick(r.vm(vmA, 100)) // commits the tenth minute
	evs := r.events()
	if len(evs) != 1 || evs[0].name != EventFiring {
		t.Fatalf("events %+v", evs)
	}
	want := map[string]string{
		"item": "vm:" + vmA, "name": "Web", "kind": "vm", "situation": "cpu_high",
		"source": "virt_management", "value": "100", "minutes": "10", "since": base.Format(time.RFC3339),
	}
	if !mapsEqual(evs[0].props, want) {
		t.Errorf("properties %v, want %v", evs[0].props, want)
	}
	if a := r.item("vm:" + vmA).Alerts; a != 1 {
		t.Errorf("alerts while firing %d", a)
	}
	_, active, err := r.s.Alerts("vm:" + vmA)
	if err != nil || len(active) != 1 || active[0].Key != "cpu_high" || active[0].Value == nil || *active[0].Value != 100 || !active[0].Since.Equal(base) {
		t.Errorf("Alerts %+v %v", active, err)
	}

	r.run(vmA, 10, 1)
	r.tick(r.vm(vmA, 10))
	evs = r.events()
	if len(evs) != 2 || evs[1].name != EventResolved || evs[1].props["situation"] != "cpu_high" || evs[1].props["since"] != want["since"] {
		t.Fatalf("events %+v", evs)
	}
	if _, ok := evs[1].props["value"]; ok {
		t.Error("a resolved carries a value")
	}
	if a := r.item("vm:" + vmA).Alerts; a != 0 {
		t.Errorf("alerts after %d", a)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestStatsMemHighAlert(t *testing.T) {
	r := newRig(t)
	high := func() libvirt.DomainStats {
		d := r.vm(vmA, 10)
		d.MemAvailableKiB, d.MemUsableKiB = u(1048576), u(52429) // 95 % used
		return d
	}
	for range 5*12 + 1 {
		r.tick(high())
	}
	evs := r.events()
	if len(evs) != 1 || evs[0].name != EventFiring || evs[0].props["situation"] != "mem_high" || evs[0].props["minutes"] != "5" || evs[0].props["value"] != "95" {
		t.Fatalf("events %+v", evs)
	}
	if a := r.item("vm:" + vmA).Alerts; a != 1 {
		t.Errorf("alerts while firing %d", a)
	}
	r.run(vmA, 10, 2) // no balloon: minutes without the mem_high series
	if n := r.count(EventResolved); n != 1 {
		t.Errorf("%d resolved", n)
	}
	if a := r.item("vm:" + vmA).Alerts; a != 0 {
		t.Errorf("alerts after %d", a)
	}
}

func TestStatsUnwritableDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if s, err := New(Options{Dir: filepath.Join(file, "stats"), Clock: stats.NewFakeClock(base), Source: &fakeSource{}}); err == nil {
		_ = s.Close()
		t.Fatal("no error for a directory under a file")
	}

	r := newRig(t)
	r.tick(r.vm(vmA, 10))
	r.tick(r.vm(vmA, 10))
	_ = r.s.store.Close() // the disk went away under the service
	for range 30 {
		r.tick(r.vm(vmA, 10))
	}
	if _, err := r.s.Series("vm:"+vmA, stats.RangeLive); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Series: %v", err)
	}
	if n := r.logged("recording VM stats failed"); n < 1 || n > 3 {
		t.Errorf("Record errors logged %d times over 2.5 minutes", n)
	}

	r = newRig(t)
	r.tick(r.vm(vmA, 10))
	_ = r.s.Close()
	_ = r.s.Close()
	logs := len(r.logs)
	r.s.Tick()
	if len(r.logs) != logs {
		t.Errorf("a tick after Close: %v", r.logs[logs:])
	}
	if _, err := r.s.Series("vm:"+vmA, stats.RangeLive); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Series after Close: %v", err)
	}
}

func TestStatsRebuildAtStart(t *testing.T) {
	r := newRig(t)
	r.run(vmA, 100, 9)
	r.tick(r.vm(vmA, 100)) // commits minute 9
	if n := r.count(EventFiring); n != 0 {
		t.Fatalf("%d firing after 9 minutes", n)
	}
	_ = r.s.Close()

	r.open()
	r.run(vmA, 100, 1)
	r.tick(r.vm(vmA, 100))
	r.run(vmA, 100, 1)
	if n := r.count(EventFiring); n != 1 {
		t.Errorf("%d firing over the whole run, want 1", n)
	}
}

func TestStatsPartialErrorKeepsRecording(t *testing.T) {
	r := newRig(t)
	// four NICs whose MACs change every second reading: 12 new series per
	// change, past the 48 a VM can ever have after the fifth
	for i := range 3 * 12 {
		d := r.vm(vmA, 50)
		set := i / 2
		for k := range 4 {
			c := u(uint64(i) * 1000)
			d.NICs = append(d.NICs, libvirt.NICStats{Index: k, MAC: "52:54:00:00:" + hex2(set) + ":" + hex2(k), RxBytes: c, TxBytes: c, RxDrop: c, TxDrop: c})
		}
		r.tick(d)
	}
	if n := r.logged("not kept"); n != 1 {
		t.Errorf("series cap logged %d times, want once", n)
	}
	if n := r.logged("recording VM stats failed"); n != 0 {
		t.Errorf("a partial error was logged as a failure %d times", n)
	}
	live := r.series("vm:"+vmA, stats.RangeLive)
	if v, ok := at(points(live, "cpu", "total"), r.clock.Now().Add(-Interval)); !ok || v != 50 {
		t.Errorf("cpu/total not recorded past the cap: %v %v", v, ok)
	}
	if it := r.item("vm:" + vmA); it.CPUPercent == nil {
		t.Error("not measured")
	}
}

func hex2(i int) string { return fmt.Sprintf("%02x", i) }

func TestStatsUnreadableHistoryIsNeverOverwritten(t *testing.T) {
	r := newRig(t)
	r.run(vmA, 10, 1)
	_ = r.s.Close()

	rings, _ := filepath.Glob(filepath.Join(r.dir, "*.ring"))
	if len(rings) != 1 {
		t.Fatalf("rings %v", rings)
	}
	target := t.TempDir()
	if err := os.Remove(rings[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, rings[0]); err != nil {
		t.Skip("no symlinks here:", err)
	}

	r.open()
	for range 3 {
		r.tick(r.vm(vmA, 10))
	}
	r.tick(stopped(vmA, libvirt.StateShutOff))
	r.run(vmA, 10, 1)

	if _, err := r.s.Series("vm:"+vmA, stats.RangeLive); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Series: %v", err)
	}
	if _, _, err := r.s.Alerts("vm:" + vmA); err != nil && !errors.Is(err, ErrUnavailable) {
		t.Errorf("Alerts: %v", err)
	}
	if err := r.s.SetRules("vm:"+vmA, stats.DefaultRules()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("SetRules: %v", err)
	}
	if got, err := os.Readlink(rings[0]); err != nil || got != target {
		t.Errorf("the history file was replaced: %q %v", got, err)
	}
	if ents, _ := os.ReadDir(target); len(ents) != 0 {
		t.Errorf("written through: %v", ents)
	}
	if n := r.logged("unreadable; that VM is not recorded"); n != 1 {
		t.Errorf("logged %d times", n)
	}
	if n := r.logged("recording VM stats failed"); n != 0 {
		t.Errorf("Record was tried %d times", n)
	}
}

func TestStatsCloseWaitsForTick(t *testing.T) {
	r := newRig(t)
	in, release := make(chan struct{}), make(chan struct{})
	r.src.onSnapshot = func() {
		in <- struct{}{}
		<-release
	}
	r.src.set([]libvirt.DomainStats{r.vm(vmA, 10)}, nil)
	ticked := make(chan struct{})
	go func() {
		r.s.Tick()
		close(ticked)
	}()
	<-in
	closed := make(chan error)
	go func() { closed <- r.s.Close() }()
	select {
	case <-closed:
		t.Fatal("Close did not wait for the tick")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-closed; err != nil {
		t.Error(err)
	}
	<-ticked
	if err := r.s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestStatsRunShutdownIsNotAnOutage(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.src.set(nil, errors.New("connection closed"))
	r.src.onSnapshot = cancel // libvirt goes away because the service stops
	done := make(chan struct{})
	go func() {
		r.s.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if n := r.logged("unreachable"); n != 0 {
		t.Errorf("shutdown logged as an outage")
	}
	if _, err := r.s.Series("vm:"+vmA, stats.RangeLive); !errors.Is(err, ErrUnavailable) {
		t.Errorf("not closed: %v", err)
	}
}

func TestStatsClockSteppedBack(t *testing.T) {
	r := newRig(t)
	r.src.titles[vmA] = [2]string{"Web", ""}
	r.run(vmA, 50, 1)
	calls := r.src.calls(vmA)
	r.clock.Advance(-time.Hour)
	r.tick(r.vm(vmA, 50))
	if r.src.calls(vmA) != calls+1 {
		t.Error("not described again after the clock stepped back")
	}
	if it := r.item("vm:" + vmA); it.CPUPercent != nil {
		t.Errorf("a rate across the step: %v", *it.CPUPercent)
	}
	r.tick(r.vm(vmA, 50))
	if it := r.item("vm:" + vmA); it.CPUPercent == nil || *it.CPUPercent != 50 {
		t.Errorf("after: %v", it.CPUPercent)
	}
}

func TestStatsDescribeIsBounded(t *testing.T) {
	old := describeBudget
	describeBudget = time.Millisecond
	t.Cleanup(func() { describeBudget = old })
	r := newRig(t)
	r.src.descDelay = 5 * time.Millisecond
	doms := func() []libvirt.DomainStats { return []libvirt.DomainStats{r.vm(vmA, 1), r.vm(vmB, 1), r.vm(vmC, 1)} }
	total := func() int { return r.src.calls(vmA) + r.src.calls(vmB) + r.src.calls(vmC) }
	for i := 1; i <= 3; i++ {
		r.tick(doms()...)
		if got := total(); got != i {
			t.Errorf("tick %d: %d Describe calls in all, want %d", i, got, i)
		}
	}
}

func TestStatsDescribesStoppedOnlyWhenNewOrChanged(t *testing.T) {
	r := newRig(t)
	r.src.titles[vmA] = [2]string{"Web", ""}
	for range 12 * 6 {
		r.tick(stopped(vmA, libvirt.StateShutOff))
	}
	if n := r.src.calls(vmA); n != 1 {
		t.Errorf("stopped: %d calls in 6 min", n)
	}
	r.s.refreshTitle(vmA)
	r.tick(stopped(vmA, libvirt.StateShutOff))
	if n := r.src.calls(vmA); n != 2 {
		t.Errorf("after refreshTitle: %d", n)
	}
	r.run(vmA, 10, 6) // started: read again, then every 5 min
	if n := r.src.calls(vmA); n != 4 {
		t.Errorf("running 6 min: %d", n)
	}
}

func TestStatsScale(t *testing.T) {
	r := newRig(t)
	d := r.vm(vmA, 10)
	d.VCPUs = n(2)
	r.tick(d)
	res := r.series("vm:"+vmA, stats.RangeLive)
	if res.Scale.CPU != "percent_of_one_core" || res.Scale.CPUAllocation == nil || *res.Scale.CPUAllocation != 2 ||
		res.Scale.MemoryLimit == nil || *res.Scale.MemoryLimit != 1048576*1024 {
		t.Errorf("%+v", res.Scale)
	}
}

func TestStatsSeriesJSON(t *testing.T) {
	r := newRig(t)
	r.tick(r.vm(vmA, 10))
	r.tick(r.vm(vmA, 10))
	b, err := json.Marshal(r.series("vm:"+vmA, stats.Range15m))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	dec := json.NewDecoder(strings.NewReader(string(b)))
	_, _ = dec.Token()
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		_ = dec.Decode(&skip)
	}
	if strings.Join(keys, ",") != "id,range,step_seconds,from,to,series,scale,notes" {
		t.Errorf("keys %v in %s", keys, b)
	}
	if !strings.Contains(string(b), `"series":[]`) || !strings.Contains(string(b), `"notes":["vm_memory_used_unavailable"]`) || !strings.Contains(string(b), `"cpu":"percent_of_one_core"`) {
		t.Errorf("%s", b)
	}
}

func TestStatsAlertsAndRules(t *testing.T) {
	r := newRig(t)
	r.run(vmA, 10, 1)
	r.tick(r.vm(vmA, 10), stopped(vmB, libvirt.StateShutOff))

	rules, active, err := r.s.Alerts("vm:" + vmA)
	if err != nil || len(active) != 0 || active == nil || !slices.Equal(rules, stats.DefaultRules()) {
		t.Fatalf("%v %v %v", rules, active, err)
	}
	if err := r.s.SetRules("vm:"+vmA, []stats.Rule{{Key: "cpu_high", Enabled: true, Threshold: 80, Minutes: 10}}); err != nil {
		t.Fatal(err)
	}
	if rules, _, _ := r.s.Alerts("vm:" + vmA); rules[0].Threshold != 80 {
		t.Errorf("%v", rules)
	}
	var re *stats.RuleError
	if err := r.s.SetRules("vm:"+vmA, []stats.Rule{{Key: "cpu_high", Enabled: true, Threshold: 0, Minutes: 10}}); !errors.As(err, &re) || re.Field != "threshold" {
		t.Errorf("bad threshold: %v", err)
	}
	// a VM never seen running is listed, but has no history to take rules
	if _, _, err := r.s.Alerts("vm:" + vmB); err != nil {
		t.Errorf("Alerts of a listed VM: %v", err)
	}
	if err := r.s.SetRules("vm:"+vmB, stats.DefaultRules()); !errors.Is(err, ErrUnknownItem) {
		t.Errorf("SetRules of a VM without history: %v", err)
	}

	// applied, but alerts.json could not be written
	tmp := filepath.Join(r.dir, "alerts.json.tmp")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.s.SetRules("vm:"+vmA, []stats.Rule{{Key: "cpu_high", Enabled: true, Threshold: 70, Minutes: 10}}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("unsaved rules: %v", err)
	}
	_ = os.Remove(tmp)
	if err := r.s.SetRules("vm:"+vmA, []stats.Rule{{Key: "cpu_high", Enabled: true, Threshold: 75, Minutes: 10}}); err != nil {
		t.Errorf("saved again: %v", err)
	}
}

// A VM with history from before a restart, stopped at its first snapshot,
// still records health/ok 0.
func TestStatsStoppedAfterRestartRecordsHealth(t *testing.T) {
	r := newRig(t)
	r.run(vmA, 10, 1)
	_ = r.s.Close()
	r.open()
	when := r.clock.Now()
	r.tick(stopped(vmA, libvirt.StateShutOff))
	if v, ok := at(points(r.series("vm:"+vmA, stats.RangeLive), "health", "ok"), when); !ok || v != 0 {
		t.Errorf("health/ok %v %v, want 0", v, ok)
	}
}

// The hourly sweep drops the history of a VM forgotten 7 days ago.
func TestStatsSweepsForgottenHistory(t *testing.T) {
	r := newRig(t)
	r.run(vmA, 10, 2)
	if err := r.s.SetRules("vm:"+vmA, []stats.Rule{{Key: "cpu_high", Enabled: true, Threshold: 80, Minutes: 10}}); err != nil {
		t.Fatal(err)
	}
	r.tick(r.vm(vmB, 10))
	r.tick(r.vm(vmB, 10))
	r.series("vm:"+vmA, stats.Range7d)
	r.clock.Advance(8 * 24 * time.Hour)
	r.tick(r.vm(vmB, 10))
	if _, err := r.s.Series("vm:"+vmA, stats.Range7d); !errors.Is(err, ErrUnknownItem) {
		t.Errorf("8 days after: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(r.dir, "alerts.json")); strings.Contains(string(b), vmA) {
		t.Errorf("its rules are kept: %s", b)
	}
}

// A VM with no CPU time can never have a rate: its reading at the end of a
// minute is recorded.
func TestStatsNoCPUTimeRecordedAtMinuteEnd(t *testing.T) {
	r := newRig(t)
	r.clock.Advance(55 * time.Second)
	when := r.clock.Now()
	r.tick(libvirt.DomainStats{UUID: vmA, State: libvirt.StateRunning})
	if _, ok := at(points(r.series("vm:"+vmA, stats.RangeLive), "health", "ok"), when); !ok {
		t.Error("not recorded")
	}
}

// After a downtime the evaluator's counters are rebuilt from the history: an
// alert of a VM not measured meanwhile ends at the start.
func TestStatsRebuildAfterDowntimeResolves(t *testing.T) {
	r := newRig(t)
	r.fire()
	_ = r.s.Close()
	r.clock.Advance(30 * time.Minute)
	r.open()
	if n := r.count(EventResolved); n != 1 {
		t.Errorf("%d resolved at the start, want 1", n)
	}
}

func TestPublisherOrderedAndBounded(t *testing.T) {
	fp := &fakePublisher{entered: make(chan struct{}, 8), block: make(chan struct{})}
	p := startPublisher(fp, 2)
	if !p.submit("a", nil) {
		t.Fatal("a")
	}
	<-fp.entered // a is being published
	if !p.submit("b", nil) || !p.submit("c", nil) {
		t.Fatal("b, c")
	}
	if p.submit("d", nil) {
		t.Error("a full queue took d")
	}
	close(fp.block)
	p.stop()
	p.stop()
	if p.submit("e", nil) {
		t.Error("a stopped publisher took e")
	}
	var names []string
	for _, g := range fp.got {
		names = append(names, g.name)
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Errorf("published %v", names)
	}
}

func TestDescribeXML(t *testing.T) {
	for _, c := range []struct{ xml, title, detail string }{
		{`<domain><name>web</name><title>Web</title><description>the shop</description></domain>`, "Web", "the shop"},
		{`<domain><name>web</name></domain>`, "web", ""},
		{`<domain xmlns:casaos="https://recasaos.github.io/xmlns/vm/1"><name>web</name><metadata><casaos:vm><casaos:os>ubuntu</casaos:os></casaos:vm></metadata></domain>`, "web", "ubuntu"},
	} {
		title, detail, err := describeXML(c.xml)
		if err != nil || title != c.title || detail != c.detail {
			t.Errorf("%s: %q %q %v", c.xml, title, detail, err)
		}
	}
	if _, _, err := describeXML("<domain"); err == nil {
		t.Error("bad XML accepted")
	}
}

// Forget frees the live slot of a VM gone for two snapshots (not one), and
// its history stays.
func TestStatsForgetFreesTheLiveSlot(t *testing.T) {
	if testing.Short() {
		t.Skip("creates 256 history files")
	}
	r := newRig(t)
	doms := make([]libvirt.DomainStats, stats.MaxItems)
	for i := range doms {
		doms[i] = libvirt.DomainStats{UUID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i), State: libvirt.StateRunning}
	}
	gone, extra := "vm:"+doms[0].UUID, libvirt.DomainStats{UUID: vmA, State: libvirt.StateRunning}
	stored := func(id string) bool { return slices.Contains(r.s.store.Items(), id) }

	r.tick(doms...)
	r.tick(doms[1:]...)                // one miss
	r.tick(append(doms[1:], extra)...) // the second: forgotten after this tick's Records
	if stored("vm:" + vmA) {
		t.Fatal("a slot was free after one missed snapshot")
	}
	if n := r.logged("too many VMs"); n != 1 {
		t.Fatalf("cap logged %d times, want 1", n)
	}
	r.tick(append(doms[1:], extra)...)
	if !stored("vm:" + vmA) {
		t.Error("the slot of the gone VM was not freed")
	}
	if !stored(gone) {
		t.Error("the history of the gone VM was dropped")
	}
}

func TestStatsRetriesAFailedForget(t *testing.T) {
	r := newRig(t)
	calls := 0
	r.s.forget = func(id string, at time.Time) error {
		if calls++; calls == 1 {
			return errors.New("disk full")
		}
		return r.s.store.Forget(id, at)
	}
	r.run(vmA, 10, 1)
	r.tick()
	r.tick()
	if calls != 1 || listed(r.s, "vm:"+vmA) {
		t.Fatalf("%d calls, listed %v", calls, listed(r.s, "vm:"+vmA))
	}
	r.tick()
	r.tick()
	if calls != 2 {
		t.Errorf("%d Forget calls, want a retry then none", calls)
	}
}

// virsh domrename works only on a shut-off domain: the new name is read
// without a state change.
func TestStatsRenamedWhileStopped(t *testing.T) {
	r := newRig(t)
	r.src.titles[vmA] = [2]string{"old", ""}
	r.tick(stopped(vmA, libvirt.StateShutOff))
	r.src.titles[vmA] = [2]string{"new", ""}
	d := stopped(vmA, libvirt.StateShutOff)
	d.Name = "new"
	r.tick(d)
	if got := r.item("vm:" + vmA).Name; got != "new" {
		t.Errorf("name %q", got)
	}
}

// A resolved event that another goroutine emits after the tick that forgot
// the VM still carries its name, for one tick.
func TestStatsLateResolvedEventIsNamed(t *testing.T) {
	r := newRig(t)
	r.src.titles[vmA] = [2]string{"Web", ""}
	r.tick(r.vm(vmA, 10))
	r.tick()
	r.tick()
	late := stats.Event{Item: "vm:" + vmA, Key: "cpu_high", State: "resolved", Since: base}
	r.s.emit(late)
	r.tick()
	r.s.emit(late)
	evs := r.events()
	if len(evs) != 2 || evs[0].props["name"] != "Web" || evs[1].props["name"] != vmA {
		t.Errorf("%+v", evs)
	}
}

// Readers run while ticks publish new views (CI runs -race).
func TestStatsConcurrentReaders(t *testing.T) {
	r := newRig(t)
	r.src.titles[vmA] = [2]string{"Web", "d"}
	r.tick(r.vm(vmA, 10), r.vm(vmB, 10))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				items, _ := r.s.Items()
				for _, it := range items {
					_, _ = json.Marshal(it)
				}
				if res, err := r.s.Series("vm:"+vmA, stats.RangeLive); err == nil {
					_, _ = json.Marshal(res)
				}
				_, _, _ = r.s.Alerts("vm:" + vmB)
				_ = r.s.Notes()
			}
		})
	}
	for i := range 60 {
		if i%7 == 0 {
			r.tick(r.vm(vmA, 10))
		} else {
			r.tick(r.vm(vmA, 10), r.vm(vmB, float64(i)))
		}
	}
	close(stop)
	wg.Wait()
}
