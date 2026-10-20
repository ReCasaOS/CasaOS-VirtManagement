package service

// Probe of what libvirt's test driver answers to the calls the statistics
// sampler relies on. Skipped unless VIRT_TEST_LIBVIRT_URI is set.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/vmstats"
)

// testDriverConn connects to the test driver, as testDriverVMs does.
func testDriverConn(t *testing.T) *libvirt.Conn {
	t.Helper()

	uri := os.Getenv("VIRT_TEST_LIBVIRT_URI")
	if uri == "" {
		t.Skip("VIRT_TEST_LIBVIRT_URI is not set")
	}

	socket := os.Getenv("VIRT_TEST_LIBVIRT_SOCKET")
	if socket == "" {
		socket = "/run/libvirt/libvirt-sock"
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	conn := libvirt.New([]string{socket}, uri)
	go conn.Run(ctx)

	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := conn.Get()
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("no connection to %s on %s: %v", uri, socket, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func isNoSupport(err error) bool {
	var e golibvirt.Error
	return errors.As(err, &e) && e.Code == uint32(golibvirt.ErrNoSupport)
}

// TestDriverAllDomainStats records whether the test driver implements
// ConnectGetAllDomainStats. When it does not, DomainGetInfo must still give a
// CPU time that grows, which is what the per-domain fallback reads.
func TestDriverAllDomainStats(t *testing.T) {
	l, err := testDriverConn(t).Get()
	if err != nil {
		t.Fatal(err)
	}

	dom, err := l.DomainLookupByName("test")
	if err != nil {
		t.Fatal(err)
	}

	records, err := l.ConnectGetAllDomainStats(nil, 63, 0)
	if err == nil {
		fields := map[string]bool{}
		for _, r := range records {
			if r.Dom.Name != "test" {
				continue
			}
			for _, p := range r.Params {
				fields[p.Field] = true
			}
		}
		t.Logf("ConnectGetAllDomainStats supported, %d records, fields of %q: %v", len(records), "test", fields)
		// the test driver answers with the state only: StatsReader completes a
		// running domain with the calls per domain (TestDriverStatsSampling)
		if !fields[golibvirt.DomainStatsStateState] {
			t.Errorf("no %q for the domain test", golibvirt.DomainStatsStateState)
		}
		return
	}
	if !isNoSupport(err) {
		t.Fatal(err)
	}

	_, _, _, _, first, err := l.DomainGetInfo(dom)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	_, _, _, _, second, err := l.DomainGetInfo(dom)
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Errorf("cpu time %d then %d: it does not grow", first, second)
	}

	t.Skipf("ConnectGetAllDomainStats: %v (DomainGetInfo cpu time %d -> %d)", err, first, second)
}

// statsBus records the events the statistics publish.
type statsBus struct {
	mu    sync.Mutex
	names []string
}

func (b *statsBus) Publish(name string, _ map[string]string) {
	b.mu.Lock()
	b.names = append(b.names, name)
	b.mu.Unlock()
}

func (b *statsBus) published() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.names)
}

func seriesPoints(s StatsSeries, chart, name string) ([]stats.Point, bool) {
	for _, d := range s.Series {
		if d.Chart == chart && d.Name == name {
			return d.Points, true
		}
	}
	return nil, false
}

// TestDriverStatsSampling samples the driver's own domain twice through the
// real source. The driver has no balloon: no memory/used, and the note says so.
func TestDriverStatsSampling(t *testing.T) {
	conn := testDriverConn(t)
	l, err := conn.Get()
	if err != nil {
		t.Fatal(err)
	}
	dom, err := l.DomainLookupByName("test")
	if err != nil {
		t.Fatal(err)
	}
	recs, allStats := l.ConnectGetAllDomainStats(nil, libvirt.AllStatsGroups, 0)
	// the driver has no batch call, or its batch lacks the CPU time: either
	// way the sampler reads the domain with the calls per domain
	fallback := isNoSupport(allStats)
	for _, rec := range recs {
		if rec.Dom.Name != "test" {
			continue
		}
		fallback = true
		for _, p := range rec.Params {
			if p.Field == golibvirt.DomainStatsCPUTime {
				fallback = false
			}
		}
	}
	_, _, memoryKiB, _, _, err := l.DomainGetInfo(dom)
	if err != nil {
		t.Fatal(err)
	}

	clock := stats.NewFakeClock(time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	st, err := NewStats(StatsOptions{Dir: t.TempDir(), Clock: clock, Source: NewConnSource(conn), Publisher: &statsBus{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	st.Tick()
	time.Sleep(time.Second) // the driver's CPU time follows the wall clock
	clock.Advance(5 * time.Second)
	st.Tick()

	id := "vm:" + uuidString(dom.UUID)
	series, err := st.Series(id, stats.RangeLive)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fallback %v, series %+v, notes %v", fallback, series.Series, series.Notes)

	if pts, ok := seriesPoints(series, "cpu", "total"); !ok || len(pts) == 0 {
		t.Error("no cpu/total")
	}
	if fallback {
		pts, _ := seriesPoints(series, "memory", "assigned")
		if n := len(pts); n == 0 || pts[n-1].Avg == nil || *pts[n-1].Avg != float64(memoryKiB)*1024 {
			t.Errorf("memory/assigned %+v, want %d bytes", pts, memoryKiB*1024)
		}
	}
	if _, ok := seriesPoints(series, "memory", "used"); ok {
		t.Error("memory/used without a balloon")
	}
	if !slices.Contains(series.Notes, "vm_memory_used_unavailable") {
		t.Errorf("notes %v", series.Notes)
	}
}

type emptySource struct{}

func (emptySource) Snapshot() ([]libvirt.DomainStats, error) { return nil, nil }
func (emptySource) Describe(string) (string, string, error)  { return "", "", errors.New("no VM") }

// TestDriverStatsNoVM: a host without VMs lists none and writes no history.
// Pure (the driver's own domain is shared with other tests), so not skipped.
func TestDriverStatsNoVM(t *testing.T) {
	dir := t.TempDir()
	clock := stats.NewFakeClock(time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	st, err := NewStats(StatsOptions{Dir: dir, Clock: clock, Source: emptySource{}, Publisher: &statsBus{}})
	if err != nil {
		t.Fatal(err)
	}
	for range 30 { // over two minutes
		st.Tick()
		clock.Advance(5 * time.Second)
	}
	if items, err := st.Items(); err != nil || len(items) != 0 {
		t.Errorf("items %v %v", items, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.ring*"))
	if err != nil || len(files) != 0 {
		t.Errorf("history files %v %v", files, err)
	}
}

// TestDriverStatsLifecycle follows a VM's real lifecycle events: a forced
// power-off and a delete raise no alert. The driver cannot make a VM crash.
func TestDriverStatsLifecycle(t *testing.T) {
	conn := testDriverConn(t)
	l, err := conn.Get()
	if err != nil {
		t.Fatal(err)
	}
	events := NewEvents(t.TempDir())
	vms, _ := testDriverVMsOn(t, conn, events)

	bus := &statsBus{}
	clock := stats.NewFakeClock(time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC))
	st, err := NewStats(StatsOptions{Dir: t.TempDir(), Clock: clock, Source: NewConnSource(conn), Publisher: bus})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seen := make(chan vmstats.LifecycleEvent, 64)
	events.OnLifecycle(func(ev vmstats.LifecycleEvent) {
		st.HandleLifecycle(ev)
		select { // the hook must not block
		case seen <- ev:
		default:
		}
	})
	go events.WatchLifecycle(t.Context(), l)
	time.Sleep(time.Second) // subscribed

	vm, _, err := vms.Create(codegen.VMCreate{Title: "stats", Vcpus: 1, MemoryMib: 256, DiskGib: 1})
	if err != nil {
		t.Fatal(err)
	}
	id := vm.Id
	t.Cleanup(func() { _, _ = vms.ChangeState(id, codegen.ForceOff); _, _ = vms.Delete(id, true) })

	wait := func(want golibvirt.DomainEventType) {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			select {
			case ev := <-seen:
				if ev.UUID == id.String() && ev.Event == want {
					return
				}
			case <-timeout:
				t.Fatalf("no %v event for %s", want, id)
			}
		}
	}
	wait(golibvirt.DomainEventStarted)
	st.Tick()

	if _, err := vms.ChangeState(id, codegen.ForceOff); err != nil {
		t.Fatal(err)
	}
	wait(golibvirt.DomainEventStopped)
	clock.Advance(5 * time.Second)
	st.Tick()
	if _, active, err := st.Alerts("vm:" + id.String()); err != nil || len(active) != 0 {
		t.Errorf("after a forced power-off: %v %v", active, err)
	}

	if _, err := vms.Delete(id, true); err != nil {
		t.Fatal(err)
	}
	wait(golibvirt.DomainEventUndefined)
	if _, active, err := st.Alerts("vm:" + id.String()); (err != nil && !errors.Is(err, ErrUnknownItem)) || len(active) != 0 {
		t.Errorf("after delete: %v %v", active, err)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if got := bus.published(); slices.Contains(got, EventResourceFiring) {
		t.Errorf("published %v", got)
	}
}
