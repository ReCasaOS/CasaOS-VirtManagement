package vmstats

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	"go.uber.org/zap"
)

var (
	ErrUnknownItem = errors.New("unknown item")
	ErrUnavailable = errors.New("stats unavailable")
)

// The MessageBus events of the cpu_high and mem_high alerts.
const (
	EventFiring   = "resource:alert-firing"
	EventResolved = "resource:alert-resolved"
)

const (
	Kind     = "vm"
	Group    = "Machines virtuelles"
	idPrefix = "vm:"

	// describeEvery: a running VM's title and description are read again this
	// often; a stopped one only when it is new or its state changed.
	describeEvery = 5 * time.Minute
	// missedSnapshots: a VM absent from this many successful snapshots in a
	// row is gone.
	missedSnapshots = 2
	maxText         = 200 // characters of a name or a detail
	publishQueue    = 64
)

// describeBudget bounds the Describe calls of a tick (a shared deadline);
// what is left waits for the next tick. A variable only so tests can shorten it.
var describeBudget = 2 * time.Second

// logError is replaced by tests: the shared logger is not initialised there.
var logError = func(msg string, err error) { logger.Error(msg, zap.Error(err)) }

// Source reads libvirt: every domain's statistics at once, and one domain's
// title and description (or an OS hint).
type Source interface {
	Snapshot() ([]libvirt.DomainStats, error)
	Describe(uuid string) (title, detail string, err error)
}

// Publisher sends a MessageBus event; *service.Events is one.
type Publisher interface {
	Publish(name string, properties map[string]string)
}

type Options struct {
	Dir       string // /var/lib/casaos/stats/virt-management
	Clock     stats.Clock
	Source    Source
	Publisher Publisher
}

// Item is a VM as the stats list shows it.
type Item struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Kind        string     `json:"kind"`
	Group       string     `json:"group"`
	State       string     `json:"state"`
	StartedAt   *time.Time `json:"started_at"`
	Detail      *string    `json:"detail"`
	CPUPercent  *float64   `json:"cpu_percent"`
	MemoryUsed  *float64   `json:"memory_used"`
	MemoryLimit *float64   `json:"memory_limit"`
	Alerts      int        `json:"alerts"`
}

type Scale struct {
	CPU           string   `json:"cpu"` // always percent_of_one_core
	CPUAllocation *float64 `json:"cpu_allocation"`
	MemoryLimit   *float64 `json:"memory_limit"`
}

// Series is a VM's history over a range, with its scale and notes.
type Series struct {
	ID string
	stats.Result
	Scale Scale
	Notes []string
}

// MarshalJSON writes id, the Result's own fields, scale and notes. Without it
// the embedded Result's MarshalJSON would be promoted and write the Result
// alone.
func (s Series) MarshalJSON() ([]byte, error) {
	type result stats.Result
	r := s.Result
	if r.Series == nil {
		r.Series = []stats.SeriesData{}
	}
	return json.Marshal(struct {
		ID string `json:"id"`
		result
		Scale Scale    `json:"scale"`
		Notes []string `json:"notes"`
	}{s.ID, result(r), s.Scale, s.Notes})
}

// Alert is an active alert of a VM; Value is nil for vm_crashed.
type Alert struct {
	Key   string    `json:"key"`
	Since time.Time `json:"since"`
	Value *float64  `json:"value"`
}

// row is what the routes read of a VM; the sampler publishes a copy per tick.
type row struct {
	id, name, detail, state string
	active                  bool // running or paused
	startedAt               *time.Time
	cpu, used               *float64 // from the last sample
	valAt                   time.Time
	vcpus, limit            *float64
	memUnavailable          bool // the last sample had no memory/used
}

// entry is the sampler's own state of a VM; only a tick touches it.
type entry struct {
	row
	libvirtState  string
	libvirtName   string
	prev          *Reading
	recorded      bool // it has history: a stopped VM records health/ok 0
	resolved      bool // Resolve ran since it stopped running
	missed        int
	describedAt   time.Time // zero: describe again
	skipLogged    bool
	partialLogged bool
}

// Stats samples every VM each Interval into a stats.Store, whose Evaluator
// raises cpu_high and mem_high.
type Stats struct {
	clock stats.Clock
	src   Source
	store *stats.Store
	ev    *stats.Evaluator
	pub   *publisher

	tickMu    sync.Mutex // a tick never runs twice at once; Close waits for it
	closed    bool       // under tickMu
	reg       map[string]*entry
	stored    map[string]bool // vm: items with history at the start
	forgets   map[string]bool // gone VMs to Forget; a failed one is tried again next tick
	forget    func(item string, at time.Time) error
	down      bool // the last snapshot failed and that was logged
	lastSweep time.Time

	mu       sync.Mutex // guards the fields below
	view     map[string]row
	gone     map[string]string // names of the VMs forgotten at the last tick, for late events
	refresh  map[string]bool   // by UUID: describe at the next tick
	starts   map[string]time.Time
	capUntil time.Time
	capLog   time.Time
	errLog   time.Time
}

// New opens the store and the evaluator in o.Dir, wires the evaluator to the
// store and rebuilds the alert counters of every VM with history.
func New(o Options) (*Stats, error) {
	st, err := stats.Open(o.Dir, o.Clock)
	if err != nil {
		return nil, err
	}
	s := &Stats{
		clock: o.Clock, src: o.Source, store: st,
		reg: map[string]*entry{}, stored: map[string]bool{}, forgets: map[string]bool{}, forget: st.Forget,
		view: map[string]row{}, gone: map[string]string{}, refresh: map[string]bool{}, starts: map[string]time.Time{},
	}
	ev, err := stats.NewEvaluator(st, o.Dir, s.emit)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	s.ev = ev
	s.pub = startPublisher(o.Publisher, publishQueue)
	st.SetOnMinute(ev.OnMinute)
	if sk := st.Skipped(); len(sk) > 0 {
		logError("some VM stats history files could not be read and are left untouched", errors.New(strings.Join(sk, ", ")))
	}
	for _, id := range st.Items() {
		if strings.HasPrefix(id, idPrefix) {
			s.stored[id] = true
			ev.Rebuild(id)
		}
	}
	return s, nil
}

// Run ticks now, then every Interval, sweeps every hour and closes the
// service when ctx ends.
func (s *Stats) Run(ctx context.Context) {
	t := time.NewTicker(Interval)
	defer t.Stop()
	defer s.Close()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick is one pass: snapshot, describe, record, forget the gone VMs.
//
// Known limit: go-libvirt calls take no context, so a Snapshot or Describe
// RPC that hangs (libvirtd stuck, not gone) blocks this tick, the next ones
// and Close until libvirt answers or the connection drops.
func (s *Stats) Tick() { s.tick(context.Background()) }

func (s *Stats) tick(ctx context.Context) {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	if s.closed {
		return
	}
	now := s.clock.Now()

	doms, err := s.src.Snapshot()
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down, not an outage
		}
		// a gap: nothing recorded, resolved or forgotten (libvirtd away is not
		// evidence that a VM stopped)
		if !s.down {
			logError("libvirt is unreachable, VM statistics are paused", err)
		}
		s.down = true
		return
	}
	s.down = false

	s.mu.Lock()
	refresh, starts := s.refresh, s.starts
	s.refresh, s.starts = map[string]bool{}, map[string]time.Time{}
	s.mu.Unlock()

	seen := make(map[string]bool, len(doms))
	deadline := time.Now().Add(describeBudget)
	for _, d := range doms {
		id := idPrefix + d.UUID
		if seen[id] {
			continue
		}
		seen[id] = true
		e := s.reg[id]
		isNew := e == nil
		if isNew {
			e = &entry{recorded: s.stored[id]}
			e.id = id
			e.name = cleanText(d.Name)
			s.reg[id] = e
		}
		e.missed = 0
		// started, stopped, renamed (virsh domrename, only while shut off),
		// redefined: read it again
		if e.libvirtState != d.State || e.libvirtName != d.Name || refresh[d.UUID] {
			e.libvirtState, e.libvirtName, e.describedAt = d.State, d.Name, time.Time{}
		}

		cs := ContractState(d.State)
		active := cs == "running" || cs == "paused"
		due := e.describedAt.IsZero() || (active && elapsed(now, e.describedAt, describeEvery))
		if due && time.Now().Before(deadline) {
			s.describe(e, d, now)
		}

		at, hasStart := starts[d.UUID]
		switch {
		case !active:
			e.startedAt = nil
		case hasStart:
			e.startedAt = &at
		case !isNew && !e.active:
			t := now
			e.startedAt = &t
		}
		e.state, e.active = cs, active
		e.vcpus, e.limit = nil, nil
		if d.VCPUs != nil {
			e.vcpus = ptr(float64(*d.VCPUs))
		}
		if d.MemAssignedKiB != nil {
			e.limit = ptr(kib(*d.MemAssignedKiB))
		}

		if !active {
			e.prev, e.cpu, e.used, e.memUnavailable = nil, nil, nil, false
			if !e.resolved {
				s.ev.Resolve(id)
				e.resolved = true
			}
			if e.recorded {
				s.record(e, now, HealthOnly())
			}
			continue
		}

		cur := Reading{At: now, S: d}
		smp, unavailable := Sample(e.prev, cur)
		e.prev, e.resolved = &cur, false
		e.cpu, e.used, e.valAt, e.memUnavailable = value(smp, "cpu/total"), value(smp, "memory/used"), now, unavailable
		// The first reading after a gap has no CPU rate. Alone in its minute (the
		// next tick falls in the next minute) it would write a minute without
		// alert/cpu_high, which ends an active cpu_high; so it is not recorded (a
		// minute without any Record keeps the alert). Cost: one 5 s live point per
		// gap. Its reading is the baseline of the next tick.
		_, rate := smp["alert/cpu_high"]
		canRate := d.CPUTimeNs != nil && d.VCPUs != nil && *d.VCPUs > 0
		if !rate && canRate && now.Add(Interval).Unix()/60 != now.Unix()/60 {
			continue
		}
		s.record(e, now, smp)
	}

	gone := map[string]string{}
	for id, e := range s.reg {
		if seen[id] {
			continue
		}
		if e.missed++; e.missed < missedSnapshots {
			continue
		}
		if !e.resolved {
			s.ev.Resolve(id)
		}
		gone[id] = e.name
		s.forgets[id] = true
		delete(s.reg, id)
	}
	for id := range s.forgets {
		if seen[id] { // back meanwhile
			delete(s.forgets, id)
			continue
		}
		// frees its live slot (256 at most); its history stays 7 days
		if err := s.forget(id, now); err != nil && !errors.Is(err, stats.ErrUnknownItem) {
			s.noteRecordError(err, now)
			continue
		}
		delete(s.forgets, id)
	}

	if elapsed(now, s.lastSweep, time.Hour) {
		s.sweep(now)
	}

	view := make(map[string]row, len(s.reg))
	for id, e := range s.reg {
		view[id] = e.row
	}
	s.mu.Lock()
	s.view, s.gone = view, gone // published: never changed afterwards
	s.mu.Unlock()
}

// sweep drops the history forgotten 7 days ago and the rules of the items
// without history. Called by the tick, hourly.
func (s *Stats) sweep(now time.Time) {
	if err := s.store.Sweep(now); err != nil {
		logError("VM stats sweep failed", err)
	}
	s.ev.Prune()
	s.lastSweep = now
}

// elapsed: d has passed since then, or the clock stepped back before it.
func elapsed(now, then time.Time, d time.Duration) bool {
	return now.Sub(then) >= d || now.Before(then)
}

// describe reads e's title and detail; a failure keeps what e had (at first
// the libvirt name and no detail) and is tried again next tick.
func (s *Stats) describe(e *entry, d libvirt.DomainStats, now time.Time) {
	title, detail, err := s.src.Describe(d.UUID)
	if err != nil {
		return
	}
	e.describedAt = now
	if e.name = cleanText(title); e.name == "" {
		e.name = cleanText(d.Name)
	}
	e.detail = cleanText(detail)
}

// record stores a sample of e. A partial error (some keys refused, the others
// stored) counts as recorded.
func (s *Stats) record(e *entry, now time.Time, smp stats.Sample) {
	if s.store.Unreadable(e.id) {
		// its history could not be read at Open: never overwritten, Series says 503
		if !e.skipLogged {
			logError("a VM stats history file is unreadable; that VM is not recorded", errors.New(e.id))
		}
		e.skipLogged = true
		return
	}
	err := s.store.Record(e.id, now, smp)
	switch {
	case err == nil:
	case stats.IsPartial(err):
		// a VM whose NICs changed MAC often can run out of series keys for good
		if !e.partialLogged {
			logError("some statistics of a VM are not kept: it has had too many devices", err)
		}
		e.partialLogged = true
	default:
		s.noteRecordError(err, now)
		return
	}
	e.recorded = true
}

// noteRecordError logs the item cap once an hour (and lists stats_capacity for
// the hour after), any other error at most once a minute.
func (s *Stats) noteRecordError(err error, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if errors.Is(err, stats.ErrTooManyItems) {
		s.capUntil = t.Add(time.Hour)
		if s.capLog.IsZero() || elapsed(t, s.capLog, time.Hour) {
			s.capLog = t
			logError("too many VMs for the stats store; some are listed but not measured", err)
		}
		return
	}
	if s.errLog.IsZero() || elapsed(t, s.errLog, time.Minute) {
		s.errLog = t
		logError("recording VM stats failed", err)
	}
}

// refreshTitle has the VM's title and description read again at the next tick
// (a domain was defined again).
func (s *Stats) refreshTitle(uuid string) {
	s.mu.Lock()
	s.refresh[uuid] = true
	s.mu.Unlock()
}

// noteStarted sets the VM's started_at at the next tick (a Started event).
func (s *Stats) noteStarted(uuid string, at time.Time) {
	s.mu.Lock()
	s.starts[uuid] = at
	s.mu.Unlock()
}

func (s *Stats) emit(e stats.Event) {
	s.mu.Lock()
	name := s.view[e.Item].name
	if name == "" { // resolved as it was forgotten, emitted after the tick
		name = s.gone[e.Item]
	}
	s.mu.Unlock()
	if name == "" { // an alert rebuilt at the start, before the first snapshot
		name = strings.TrimPrefix(e.Item, idPrefix)
	}
	minutes := 0
	for _, r := range s.ev.Rules(e.Item) {
		if r.Key == e.Key {
			minutes = r.Minutes
		}
	}
	event := EventResolved
	properties := map[string]string{
		"item":      e.Item,
		"name":      name,
		"kind":      Kind,
		"situation": e.Key,
		"minutes":   strconv.Itoa(minutes),
		"since":     e.Since.UTC().Format(time.RFC3339),
		"source":    "virt_management",
	}
	if e.State == "firing" {
		event = EventFiring
		if !math.IsNaN(e.Value) && !math.IsInf(e.Value, 0) {
			properties["value"] = strconv.FormatFloat(e.Value, 'f', 0, 64)
		}
	}
	if !s.pub.submit(event, properties) {
		logError("a VM resource alert was dropped, the message bus queue is full or stopped", errors.New(e.Item+" "+e.Key+" "+e.State))
	}
}

// Items lists the VMs of the last successful snapshot, by name.
func (s *Stats) Items() []Item {
	s.mu.Lock()
	view := s.view
	s.mu.Unlock()
	now := s.clock.Now()
	out := make([]Item, 0, len(view))
	for _, r := range view {
		it := Item{ID: r.id, Name: r.name, Kind: Kind, Group: Group, State: r.state, StartedAt: r.startedAt, MemoryLimit: r.limit}
		if r.detail != "" {
			d := r.detail
			it.Detail = &d
		}
		if !r.valAt.IsZero() && now.Sub(r.valAt) <= MaxGap {
			it.CPUPercent, it.MemoryUsed = r.cpu, r.used
		}
		it.Alerts = len(s.ev.Active(r.id))
		out = append(out, it)
	}
	slices.SortFunc(out, func(a, b Item) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.ID, b.ID))
	})
	return out
}

// Notes: stats_capacity during the hour after the item cap was hit.
func (s *Stats) Notes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clock.Now().Before(s.capUntil) {
		return []string{"stats_capacity"}
	}
	return []string{}
}

// Series returns the history of id over r, without the alert series.
func (s *Stats) Series(id string, r stats.Range) (Series, error) {
	if _, ok := stats.ParseRange(string(r)); !ok {
		return Series{}, stats.ErrBadRange
	}
	s.mu.Lock()
	rw, live := s.view[id]
	s.mu.Unlock()
	res, err := s.store.Query(id, r)
	switch {
	case errors.Is(err, stats.ErrUnknownItem):
		if s.store.Unreadable(id) {
			return Series{}, ErrUnavailable
		}
		if !live {
			return Series{}, ErrUnknownItem
		}
		res = emptyResult(r, s.clock.Now()) // listed, nothing stored yet
	case err != nil:
		return Series{}, ErrUnavailable
	}
	res.Series = slices.DeleteFunc(slices.Clone(res.Series), func(d stats.SeriesData) bool { return d.Chart == "alert" })
	out := Series{ID: id, Result: res, Scale: Scale{CPU: "percent_of_one_core"}, Notes: []string{}}
	if live {
		out.Scale.CPUAllocation, out.Scale.MemoryLimit = rw.vcpus, rw.limit
		if rw.memUnavailable { // false for a stopped VM
			out.Notes = append(out.Notes, "vm_memory_used_unavailable")
		}
	}
	return out, nil
}

// emptyResult is what Query answers for r at now when nothing is stored: the
// range's step and window, no series.
func emptyResult(r stats.Range, now time.Time) stats.Result {
	step, n := int64(5), int64(stats.LiveSlots)
	switch r {
	case stats.Range15m:
		step, n = 60, 15
	case stats.Range1h:
		step, n = 60, 60
	case stats.Range24h:
		step, n = 300, 288
	case stats.Range7d:
		step, n = 1800, 336
	}
	floor := func(x, m int64) int64 { return x - x%m }
	to := floor(now.Unix(), step)
	if r != stats.RangeLive {
		to = floor(floor(now.Unix(), 60)-60, step)
	}
	return stats.Result{Range: r, StepSeconds: int(step), From: to - (n-1)*step, To: to, Series: []stats.SeriesData{}}
}

// lookup: nil for a VM listed or with history, ErrUnavailable for one whose
// history could not be read at the start, ErrUnknownItem otherwise.
func (s *Stats) lookup(id string, listedToo bool) error {
	s.mu.Lock()
	_, live := s.view[id]
	s.mu.Unlock()
	switch {
	case (listedToo && live) || slices.Contains(s.store.Items(), id):
		return nil
	case s.store.Unreadable(id):
		return ErrUnavailable
	}
	return ErrUnknownItem
}

// Alerts returns the rules and the active alerts of id.
func (s *Stats) Alerts(id string) ([]stats.Rule, []Alert, error) {
	if err := s.lookup(id, true); err != nil {
		return nil, nil, err
	}
	active := []Alert{}
	for _, e := range s.ev.Active(id) {
		v := e.Value
		active = append(active, Alert{Key: e.Key, Since: e.Since, Value: &v})
	}
	return s.ev.Rules(id), active, nil
}

// SetRules updates the rules sent for id: the Evaluator's *stats.RuleError on
// a bad value, ErrUnavailable when they are applied but could not be saved.
// Only a VM with history takes rules: Prune drops the rules of any other.
func (s *Stats) SetRules(id string, rules []stats.Rule) error {
	if err := s.lookup(id, false); err != nil {
		return err
	}
	if err := s.ev.SetRules(id, rules); err != nil {
		return err
	}
	if s.ev.LastError() != nil {
		return ErrUnavailable
	}
	return nil
}

// Close waits for a tick in flight, closes the store, then publishes the
// queued events; later calls return nil.
func (s *Stats) Close() error {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	err := s.store.Close()
	s.pub.stop()
	return err
}

// cleanText drops control and format characters (bidi overrides included)
// and invalid UTF-8, then cuts to maxText characters.
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if r := []rune(s); len(r) > maxText {
		s = string(r[:maxText])
	}
	return strings.TrimSpace(s)
}

func ptr(v float64) *float64 { return &v }

func value(s stats.Sample, key string) *float64 {
	if v, ok := s[key]; ok {
		return &v
	}
	return nil
}

// publisher is one goroutine fed by a bounded channel, so that a firing and
// the resolved that follows it reach the bus in the order they were emitted,
// and the sampler never waits on the bus. Full: the newest event is dropped.
type publisher struct {
	to     Publisher
	mu     sync.Mutex
	closed bool
	jobs   chan job
	done   chan struct{}
}

type job struct {
	name       string
	properties map[string]string
}

func startPublisher(to Publisher, size int) *publisher {
	p := &publisher{to: to, jobs: make(chan job, size), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for j := range p.jobs {
			if p.to != nil {
				p.to.Publish(j.name, j.properties)
			}
		}
	}()
	return p
}

// submit never blocks; false when the event was dropped.
func (p *publisher) submit(name string, properties map[string]string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	select {
	case p.jobs <- job{name, properties}:
		return true
	default:
		return false
	}
}

// stop publishes what is queued, then ends the goroutine; again does nothing.
func (p *publisher) stop() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.jobs)
	}
	p.mu.Unlock()
	<-p.done
}
