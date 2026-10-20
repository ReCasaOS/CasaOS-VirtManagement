package vmstats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	golibvirt "github.com/digitalocean/go-libvirt"
)

const vmD = "abcdefab-cdef-4abc-8def-abcdefabcdef"

func (r *rig) life(uuid string, ev golibvirt.DomainEventType, detail int32) {
	r.s.HandleLifecycle(LifecycleEvent{UUID: uuid, Name: "vm-" + uuid[:4], Event: ev, Detail: detail})
}

// crashEvents are the published vm_crashed events, as "firing"/"resolved".
func (r *rig) crashEvents() []published {
	var out []published
	for _, e := range r.events() {
		if e.props["situation"] == crashKey {
			out = append(out, e)
		}
	}
	return out
}

func (r *rig) savedCrashes() map[string]crash {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, crashFile))
	if err != nil {
		r.t.Fatal(err)
	}
	var m map[string]crash
	if err := json.Unmarshal(b, &m); err != nil {
		r.t.Fatal(err)
	}
	return m
}

func crashedVM(id string) libvirt.DomainStats {
	d := stopped(id, libvirt.StateShutOff)
	d.Crashed = true
	return d
}

func TestCrashTable(t *testing.T) {
	S, C := golibvirt.DomainEventStopped, golibvirt.DomainEventCrashed
	cases := []struct {
		ev     golibvirt.DomainEventType
		detail int32
		fires  bool
	}{
		{S, 2, true}, {S, 5, true}, {C, 0, true}, {C, 1, true}, {C, 99, true},
		{S, 0, false}, {S, 1, false}, {S, 3, false}, {S, 4, false}, {S, 6, false}, {S, 7, false},
		{S, 99, false}, {S, -1, false},
		{golibvirt.DomainEventShutdown, 0, false}, {golibvirt.DomainEventSuspended, 0, false},
		{golibvirt.DomainEventResumed, 0, false}, {golibvirt.DomainEventPmsuspended, 0, false},
		{golibvirt.DomainEventDefined, 0, false}, {golibvirt.DomainEventStarted, 2, false},
		{golibvirt.DomainEventType(42), 2, false}, {golibvirt.DomainEventType(-1), 5, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%d,%d", c.ev, c.detail), func(t *testing.T) {
			r := newRig(t)
			r.src.titles[vmA] = [2]string{"Web server", ""}
			r.tick(r.vm(vmA, 10))
			r.life(vmA, c.ev, c.detail)
			evs := r.crashEvents()
			if !c.fires {
				if len(evs) != 0 {
					t.Fatalf("events %+v", evs)
				}
				return
			}
			if len(evs) != 1 || evs[0].name != EventFiring {
				t.Fatalf("events %+v", evs)
			}
			want := map[string]string{
				"item": "vm:" + vmA, "name": "Web server", "kind": "vm", "situation": "vm_crashed",
				"source": "virt_management", "since": base.Add(Interval).Format(time.RFC3339),
			}
			if !mapsEqual(evs[0].props, want) {
				t.Errorf("properties %v, want %v", evs[0].props, want)
			}
		})
	}
}

func TestCrashFiresOnce(t *testing.T) {
	r := newRig(t)
	r.tick(r.vm(vmA, 10))
	r.life(vmA, golibvirt.DomainEventCrashed, 0)
	r.clock.Advance(time.Second)
	r.life(vmA, golibvirt.DomainEventStopped, 2)
	r.tick(crashedVM(vmA))
	if evs := r.crashEvents(); len(evs) != 1 {
		t.Fatalf("events %+v", evs)
	}
}

func TestCrashResolvedOnStart(t *testing.T) {
	r := newRig(t)
	r.tick(r.vm(vmA, 10))
	r.life(vmA, golibvirt.DomainEventStopped, 5)
	r.tick(stopped(vmA, libvirt.StateShutOff))
	r.clock.Advance(time.Minute)
	started := r.clock.Now()
	r.life(vmA, golibvirt.DomainEventStarted, 0)
	r.clock.Advance(time.Second) // the event's time, not the tick's
	evs := r.crashEvents()
	if len(evs) != 2 || evs[1].name != EventResolved {
		t.Fatalf("events %+v", evs)
	}
	if _, ok := evs[1].props["value"]; ok || evs[1].props["since"] != evs[0].props["since"] || evs[1].props["name"] != "vm-1111" {
		t.Errorf("resolved %v", evs[1].props)
	}
	r.tick(r.vm(vmA, 10))
	if it := r.item("vm:" + vmA); it.StartedAt == nil || !it.StartedAt.Equal(started) || it.Alerts != 0 {
		t.Errorf("item %+v", it)
	}
	r.life(vmA, golibvirt.DomainEventStarted, 0)
	if n := len(r.crashEvents()); n != 2 {
		t.Errorf("%d events after a second start", n)
	}
	if m := r.savedCrashes(); len(m) != 0 {
		t.Errorf("saved %v", m)
	}
}

func TestCrashResolvedOnUndefine(t *testing.T) {
	r := newRig(t)
	var forgot []string
	r.s.forget = func(item string, at time.Time) error {
		forgot = append(forgot, item)
		return r.s.store.Forget(item, at)
	}
	r.run(vmA, 100, 10)
	r.tick(r.vm(vmA, 100)) // cpu_high fires
	r.life(vmA, golibvirt.DomainEventCrashed, 0)
	r.life(vmA, golibvirt.DomainEventUndefined, 0)

	var got []string
	for _, e := range r.events() {
		got = append(got, e.name+" "+e.props["situation"])
	}
	want := []string{EventFiring + " cpu_high", EventFiring + " vm_crashed", EventResolved + " vm_crashed", EventResolved + " cpu_high"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events %v, want %v", got, want)
	}
	if len(forgot) != 1 || forgot[0] != "vm:"+vmA {
		t.Errorf("forgot %v", forgot)
	}
	if res := r.series("vm:"+vmA, stats.Range1h); len(points(res, "cpu", "total")) == 0 {
		t.Errorf("series gone: %+v", res)
	}
}

func TestCrashResolvedWhenTickSeesRunning(t *testing.T) {
	r := newRig(t)
	r.tick(r.vm(vmA, 10))
	r.life(vmA, golibvirt.DomainEventCrashed, 0)
	r.tick(r.vm(vmA, 10)) // started again, the event was missed
	evs := r.crashEvents()
	if len(evs) != 2 || evs[1].name != EventResolved {
		t.Fatalf("events %+v", evs)
	}
}

func TestCrashEventDuringSnapshotWins(t *testing.T) {
	r := newRig(t)
	r.tick(r.vm(vmA, 10))
	// the snapshot says running, the crash happens right after it
	r.src.onSnapshot = func() { r.life(vmA, golibvirt.DomainEventCrashed, 0) }
	r.tick(r.vm(vmA, 10))
	r.src.onSnapshot = nil
	if evs := r.crashEvents(); len(evs) != 1 {
		t.Fatalf("events %+v", evs)
	}
	// and a start right after a snapshot that still says crashed
	r.src.onSnapshot = func() { r.life(vmA, golibvirt.DomainEventStarted, 0) }
	r.tick(crashedVM(vmA))
	r.src.onSnapshot = nil
	if evs := r.crashEvents(); len(evs) != 2 || evs[1].name != EventResolved {
		t.Fatalf("events %+v", evs)
	}
	// a crash of a VM absent from the snapshot taken as it happened: that
	// snapshot does not count as a miss
	r.src.onSnapshot = func() { r.life(vmC, golibvirt.DomainEventCrashed, 0) }
	r.tick(r.vm(vmA, 10))
	r.src.onSnapshot = nil
	r.tick(r.vm(vmA, 10))
	if evs := r.crashEvents(); len(evs) != 3 {
		t.Fatalf("events %+v", evs)
	}
	r.tick(r.vm(vmA, 10))
	if evs := r.crashEvents(); len(evs) != 4 || evs[3].name != EventResolved {
		t.Fatalf("events %+v", evs)
	}
}

func TestCrashResolvedAfterTwoSnapshotsWithoutVM(t *testing.T) {
	r := newRig(t)
	r.tick(r.vm(vmA, 10), r.vm(vmB, 10))
	r.life(vmA, golibvirt.DomainEventStopped, 2)
	r.tick(r.vm(vmB, 10))
	r.tick(stopped(vmA, libvirt.StateShutOff), r.vm(vmB, 10)) // back: the count starts again
	r.tick(r.vm(vmB, 10))
	r.fail(Interval) // an outage counts for nothing
	if n := len(r.crashEvents()); n != 1 {
		t.Fatalf("%d events after one snapshot without the VM", n)
	}
	r.tick(r.vm(vmB, 10))
	if evs := r.crashEvents(); len(evs) != 2 || evs[1].name != EventResolved || evs[1].props["name"] != "vm-1111" {
		t.Fatalf("events %+v", evs)
	}
	// a crash of a VM never listed (transient, gone at once) ends the same way
	r.life(vmC, golibvirt.DomainEventCrashed, 0)
	r.tick(r.vm(vmB, 10))
	r.tick(r.vm(vmB, 10))
	if evs := r.crashEvents(); len(evs) != 4 || evs[3].name != EventResolved || evs[3].props["name"] != "vm-3333" {
		t.Fatalf("events %+v", evs)
	}
}

func TestCrashNoResolvedWithoutFiring(t *testing.T) {
	r := newRig(t)
	r.tick(r.vm(vmA, 10))
	r.life(vmA, golibvirt.DomainEventStarted, 0)
	r.life(vmA, golibvirt.DomainEventStopped, 1)
	r.life(vmA, golibvirt.DomainEventUndefined, 0)
	r.tick()
	r.tick()
	if evs := r.events(); len(evs) != 0 {
		t.Fatalf("events %+v", evs)
	}
}

func TestCrashSurvivesRestart(t *testing.T) {
	r := newRig(t)
	r.tick(r.vm(vmA, 10))
	r.life(vmA, golibvirt.DomainEventCrashed, 0)
	since := r.clock.Now()
	_ = r.s.Close()
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(r.dir, crashFile)); err != nil || fi.Mode().Perm() != 0o640 {
			t.Errorf("crashed.json %v %v", fi, err)
		}
	}

	r.clock.Advance(time.Hour)
	r.open()
	r.tick(crashedVM(vmA))
	_, active, err := r.s.Alerts("vm:" + vmA)
	if err != nil || len(active) != 1 || active[0].Key != crashKey || active[0].Value != nil || !active[0].Since.Equal(since) {
		t.Fatalf("Alerts %+v %v", active, err)
	}
	r.life(vmA, golibvirt.DomainEventStarted, 0)
	evs := r.crashEvents()
	if len(evs) != 2 || evs[0].name != EventFiring || evs[1].name != EventResolved || evs[1].props["since"] != since.Format(time.RFC3339) {
		t.Fatalf("events after the restart %+v", evs)
	}
}

func TestCrashWhileDownFiresAtStart(t *testing.T) {
	r := newRig(t)
	r.tick(crashedVM(vmA), stopped(vmB, libvirt.StateCrashed), stopped(vmC, libvirt.StateShutOff))
	r.tick(crashedVM(vmA), stopped(vmB, libvirt.StateCrashed), stopped(vmC, libvirt.StateShutOff))
	evs := r.crashEvents()
	if len(evs) != 2 || evs[0].name != EventFiring || evs[1].name != EventFiring || evs[0].props["item"] == evs[1].props["item"] {
		t.Fatalf("events %+v", evs)
	}
	for _, e := range evs {
		if e.props["item"] == "vm:"+vmC {
			t.Errorf("a plain shut-off VM fired")
		}
	}
}

func TestCrashHostileFile(t *testing.T) {
	big := `{"pad":{"name":"` + strings.Repeat("x", maxCrashFile) + `"}}` // valid, too large
	m := map[string]any{
		vmA:                     map[string]any{"since": base.Format(time.RFC3339), "name": "a‮b\x00c" + strings.Repeat("é", 300)},
		strings.ToUpper(vmD):    map[string]any{"since": base.Format(time.RFC3339), "name": "upper"},
		"urn:uuid:" + vmB:       map[string]any{"since": base.Format(time.RFC3339), "name": "urn"},
		"not-a-uuid":            map[string]any{"since": base.Format(time.RFC3339), "name": "bad"},
		vmC:                     map[string]any{"name": "no since"},
		"../../etc/passwd/../x": map[string]any{"since": base.Format(time.RFC3339)},
	}
	for i := range 300 {
		m[fmt.Sprintf("44444444-4444-4444-4444-%012d", i)] = map[string]any{"since": base.Format(time.RFC3339)}
	}
	good, _ := json.Marshal(m)

	for name, content := range map[string]string{"garbage": "{nope", "null": "null", "array": "[1,2]", "big": big, "hostile": string(good)} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			_ = r.s.Close()
			if err := os.WriteFile(filepath.Join(r.dir, crashFile), []byte(content), 0o640); err != nil {
				t.Fatal(err)
			}
			r.open()
			if name != "hostile" {
				if len(r.s.crashed) != 0 {
					t.Errorf("loaded %v", r.s.crashed)
				}
				if name != "null" && r.logged("crashed") != 1 {
					t.Errorf("logs %v", r.logs)
				}
				return
			}
			if len(r.s.crashed) != stats.MaxItems {
				t.Errorf("%d loaded", len(r.s.crashed))
			}
			c, ok := r.s.crashed[vmA]
			if !ok || c.Name != "abc"+strings.Repeat("é", maxText-3) || !c.Since.Equal(base) {
				t.Errorf("vmA %+v %v", c, ok)
			}
			for k := range r.s.crashed {
				if k != vmA && !strings.HasPrefix(k, "44444444-") {
					t.Errorf("loaded %q", k)
				}
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		r := newRig(t)
		_ = r.s.Close()
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		if err := os.WriteFile(target, good, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(r.dir, crashFile)); err != nil {
			t.Skip("no symlinks here:", err)
		}
		r.open()
		if len(r.s.crashed) != 0 || r.logged("crashed") != 1 {
			t.Errorf("loaded %d, logs %v", len(r.s.crashed), r.logs)
		}
	})
	t.Run("not a regular file", func(t *testing.T) {
		r := newRig(t)
		_ = r.s.Close()
		if err := os.Mkdir(filepath.Join(r.dir, crashFile), 0o750); err != nil {
			t.Fatal(err)
		}
		r.open()
		r.tick(r.vm(vmA, 10))
		r.life(vmA, golibvirt.DomainEventCrashed, 0) // cannot be saved: logged once, still published
		r.life(vmA, golibvirt.DomainEventStarted, 0)
		if len(r.crashEvents()) != 2 || r.logged("crashed") != 2 {
			t.Errorf("events %+v logs %v", r.crashEvents(), r.logs)
		}
	})
}

func TestCrashShownInItemsAndAlerts(t *testing.T) {
	r := newRig(t)
	r.tick(r.vm(vmA, 10))
	r.life(vmA, golibvirt.DomainEventCrashed, 0)
	r.tick(crashedVM(vmA))
	if it := r.s.Items(); len(it) != 1 || it[0].Alerts != 1 {
		t.Fatalf("items %+v", it)
	}
	_, active, err := r.s.Alerts("vm:" + vmA)
	if err != nil || len(active) != 1 || active[0].Key != "vm_crashed" || active[0].Value != nil || !active[0].Since.Equal(base.Add(Interval)) {
		t.Fatalf("Alerts %+v %v", active, err)
	}
	if b, _ := json.Marshal(active[0]); !strings.Contains(string(b), `"value":null`) {
		t.Errorf("json %s", b)
	}
}

func TestCrashNameCleaned(t *testing.T) {
	r := newRig(t)
	r.s.HandleLifecycle(LifecycleEvent{UUID: strings.ToUpper(vmD), Name: "bad‮\x1b[31mname" + strings.Repeat("z", 500), Event: golibvirt.DomainEventCrashed})
	r.s.HandleLifecycle(LifecycleEvent{UUID: "not a uuid", Name: "x", Event: golibvirt.DomainEventCrashed})
	r.s.HandleLifecycle(LifecycleEvent{UUID: vmB, Name: "\x00​", Event: golibvirt.DomainEventCrashed})
	evs := r.crashEvents()
	if len(evs) != 2 {
		t.Fatalf("events %+v", evs)
	}
	if n := evs[0].props["name"]; n != "bad[31mname"+strings.Repeat("z", maxText-11) || evs[0].props["item"] != "vm:"+vmD {
		t.Errorf("first %v", evs[0].props)
	}
	if evs[1].props["name"] != vmB {
		t.Errorf("empty name %v", evs[1].props)
	}
}

func TestCrashFloodIsBounded(t *testing.T) {
	r := newRig(t)
	r.pub.block = make(chan struct{})
	r.tick(r.vm(vmA, 10))
	for i := range 200 {
		ev := golibvirt.DomainEventCrashed
		if i%2 == 1 {
			ev = golibvirt.DomainEventStarted
		}
		r.life(vmA, ev, int32(i))
	}
	for i := range 400 {
		r.life(fmt.Sprintf("55555555-5555-5555-5555-%012d", i), golibvirt.DomainEventStopped, 2)
	}
	if n := len(r.s.crashed); n != stats.MaxItems {
		t.Errorf("%d crashes kept", n)
	}
	if r.logged("dropped") == 0 {
		t.Errorf("no drop logged")
	}
	close(r.pub.block)
	if n := len(r.savedCrashes()); n != stats.MaxItems {
		t.Errorf("%d saved", n)
	}
}

func TestCrashDefinedRefreshesTitle(t *testing.T) {
	r := newRig(t)
	r.src.titles[vmA] = [2]string{"A", ""}
	r.tick(stopped(vmA, libvirt.StateShutOff))
	before := r.src.calls(vmA)
	r.tick(stopped(vmA, libvirt.StateShutOff))
	r.life(vmA, golibvirt.DomainEventDefined, 1)
	r.tick(stopped(vmA, libvirt.StateShutOff))
	if r.src.calls(vmA) != before+1 {
		t.Errorf("describe calls %d, before %d", r.src.calls(vmA), before)
	}
}

func TestStatsSweepKeepsOtherFiles(t *testing.T) {
	r := newRig(t)
	r.run(vmA, 100, 10)
	r.tick(r.vm(vmA, 100)) // alerts.json
	r.life(vmA, golibvirt.DomainEventCrashed, 0)
	tmp := filepath.Join(r.dir, crashFile+".tmp")
	if err := os.WriteFile(tmp, []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(2 * time.Hour)
	r.s.sweep(r.clock.Now())
	for _, f := range []string{crashFile, crashFile + ".tmp", "alerts.json"} {
		if _, err := os.Stat(filepath.Join(r.dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
