package vmstats

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
)

const (
	crashKey     = "vm_crashed"
	crashFile    = "crashed.json" // next to the store's files; Sweep leaves it alone
	maxCrashFile = 1 << 20
)

// LifecycleEvent is a libvirt domain lifecycle event.
type LifecycleEvent struct {
	UUID   string
	Name   string // libvirt's name, for a VM not listed yet
	Event  golibvirt.DomainEventType
	Detail int32
}

// crash is a firing vm_crashed alert, kept in crashed.json by UUID.
type crash struct {
	Since  time.Time `json:"since"`
	Name   string    `json:"name"`
	missed int       // successful snapshots without the VM, in a row
}

// isCrash: the guest crashed (any detail), or stopped by a crash or a failure.
// An asked shutdown, a destroy, a save, a migration or a snapshot restore is
// not one.
func isCrash(ev golibvirt.DomainEventType, detail int32) bool {
	switch ev {
	case golibvirt.DomainEventCrashed:
		return true
	case golibvirt.DomainEventStopped:
		d := golibvirt.DomainEventStoppedDetailType(detail)
		return d == golibvirt.DomainEventStoppedCrashed || d == golibvirt.DomainEventStoppedFailed
	}
	return false
}

// HandleLifecycle raises vm_crashed on a crash, ends it on Started or
// Undefined; Undefined also ends the VM's other alerts and forgets it, Defined
// has its title read again. Unknown events and bad UUIDs are ignored.
func (s *Stats) HandleLifecycle(ev LifecycleEvent) {
	u, err := uuid.Parse(ev.UUID)
	if err != nil {
		return
	}
	id := u.String()
	now := s.clock.Now()

	s.mu.Lock()
	s.touched[id] = true // this tick's snapshot may be older than the event
	switch {
	case isCrash(ev.Event, ev.Detail):
		name := s.view[idPrefix+id].name
		if name == "" {
			name = cleanText(ev.Name)
		}
		s.fireCrash(id, name, now)
	case ev.Event == golibvirt.DomainEventStarted:
		s.resolveCrash(id)
		s.starts[id] = now
	case ev.Event == golibvirt.DomainEventUndefined:
		s.resolveCrash(id)
	case ev.Event == golibvirt.DomainEventDefined:
		s.refresh[id] = true
	}
	s.mu.Unlock()

	if ev.Event == golibvirt.DomainEventUndefined {
		s.ev.Resolve(idPrefix + id) // emits: s.mu must be free
		if err := s.forget(idPrefix+id, now); err != nil && !errors.Is(err, stats.ErrUnknownItem) {
			s.noteRecordError(err, now) // the tick forgets it again after two snapshots without it
		}
	}
}

// crashTick, under the tick: a VM libvirt shows crashed without an alert (it
// crashed while this service or its event stream was away) fires; a firing VM
// that runs again, or is absent from two successful snapshots in a row,
// resolves. A VM with an event since this tick began is left to that event.
func (s *Stats) crashTick(doms []libvirt.DomainStats, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	present := make(map[string]bool, len(doms))
	for _, d := range doms {
		present[d.UUID] = true
		if s.touched[d.UUID] {
			continue
		}
		_, firing := s.crashed[d.UUID]
		cs := ContractState(d.State)
		switch {
		case firing && (cs == "running" || cs == "paused"):
			s.resolveCrash(d.UUID)
		case !firing && (cs == "crashed" || d.Crashed):
			name := ""
			if e := s.reg[idPrefix+d.UUID]; e != nil {
				name = e.name
			}
			s.fireCrash(d.UUID, name, now)
		}
	}
	for id, c := range s.crashed {
		switch {
		case present[id] || s.touched[id]:
			c.missed = 0
		case c.missed+1 >= missedSnapshots:
			s.resolveCrash(id)
			continue
		default:
			c.missed++
		}
		s.crashed[id] = c
	}
}

// fireCrash and resolveCrash run under s.mu, so that the firing and the
// resolved of a VM reach the publisher in order.
func (s *Stats) fireCrash(id, name string, now time.Time) {
	if _, ok := s.crashed[id]; ok {
		return
	}
	if len(s.crashed) >= stats.MaxItems {
		return // as many crashed VMs as the store has items: not a real host
	}
	if name == "" {
		name = id
	}
	c := crash{Since: now.UTC().Truncate(time.Second), Name: name}
	s.crashed[id] = c
	s.saveCrashes()
	s.submitCrash(EventFiring, id, c)
}

func (s *Stats) resolveCrash(id string) {
	c, ok := s.crashed[id]
	if !ok {
		return
	}
	delete(s.crashed, id)
	s.saveCrashes()
	s.submitCrash(EventResolved, id, c)
}

// submitCrash: no value and no minutes for vm_crashed.
func (s *Stats) submitCrash(event, id string, c crash) {
	properties := map[string]string{
		"item":      idPrefix + id,
		"name":      c.Name,
		"kind":      Kind,
		"situation": crashKey,
		"since":     c.Since.Format(time.RFC3339),
		"source":    "virt_management",
	}
	if !s.pub.submit(event, properties) {
		logError("a VM resource alert was dropped, the message bus queue is full or stopped", errors.New(idPrefix+id+" "+crashKey))
	}
}

// saveCrashes writes crashed.json through a temporary file; a failure is
// logged once until a write succeeds again (the alerts stay in memory).
func (s *Stats) saveCrashes() {
	b, err := json.Marshal(s.crashed)
	if err == nil {
		err = writeFileAtomic(filepath.Join(s.dir, crashFile), b)
	}
	if err != nil && !s.crashSaveFailed {
		logError("the crashed VMs could not be saved in crashed.json", err)
	}
	s.crashSaveFailed = err != nil
}

func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// loadCrashes reads crashed.json: absent is empty; a file that is not
// regular, too large or not JSON is an error (and nothing is loaded). Entries
// with a non-canonical UUID or no time are dropped, names cleaned, at most
// stats.MaxItems kept.
func loadCrashes(dir string) (map[string]crash, error) {
	out := map[string]crash{}
	path := filepath.Join(dir, crashFile)
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return out, nil
	case err != nil:
		return out, err
	case !fi.Mode().IsRegular():
		return out, errors.New("crashed.json is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	// a larger file is cut, so it is not valid JSON
	b, err := io.ReadAll(io.LimitReader(f, maxCrashFile))
	if err != nil {
		return out, err
	}
	var raw map[string]crash
	if err := json.Unmarshal(b, &raw); err != nil {
		return out, err
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		c := raw[k]
		if u, err := uuid.Parse(k); err != nil || u.String() != k || c.Since.IsZero() {
			continue
		}
		if len(out) == stats.MaxItems {
			break
		}
		c.Since = c.Since.UTC()
		if c.Name = cleanText(c.Name); c.Name == "" {
			c.Name = k
		}
		out[k] = c
	}
	return out, nil
}
