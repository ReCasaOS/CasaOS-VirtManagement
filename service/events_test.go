package service

import (
	"slices"
	"testing"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/vmstats"
	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
)

func TestEventTypesIncludeResource(t *testing.T) {
	want := []string{"item", "name", "kind", "situation", "value", "since", "source", "minutes"}
	for _, name := range []string{"resource:alert-firing", "resource:alert-resolved"} {
		i := slices.IndexFunc(EventTypes, func(e external.EventType) bool { return e.Name == name })
		if i < 0 {
			t.Fatalf("%s not registered", name)
		}
		var got []string
		for _, p := range EventTypes[i].PropertyTypeList {
			got = append(got, p.Name)
		}
		if !slices.Equal(got, want) || EventTypes[i].SourceID != "virt-management" {
			t.Errorf("%s: %v from %q", name, got, EventTypes[i].SourceID)
		}
	}
}

func TestLifecycleEventOf(t *testing.T) {
	id := "6f1c2a3b-4d5e-4f60-8a7b-9c0d1e2f3a4b"
	got := lifecycleEventOf(golibvirt.DomainEventLifecycleMsg{
		Dom:   golibvirt.Domain{Name: "web", UUID: golibvirt.UUID(uuid.MustParse(id))},
		Event: int32(golibvirt.DomainEventStopped), Detail: 5,
	})
	if got != (vmstats.LifecycleEvent{UUID: id, Name: "web", Event: golibvirt.DomainEventStopped, Detail: 5}) {
		t.Errorf("%+v", got)
	}
}

func TestLifecycleHookNotComplainedOfWhenStatsDisabled(t *testing.T) {
	logged := 0
	old := logNoLifecycleHook
	logNoLifecycleHook = func() { logged++ }
	t.Cleanup(func() { logNoLifecycleHook = old })

	e := NewEvents(t.TempDir())
	e.StatsDisabled()
	e.checkLifecycleHook()
	if logged != 0 {
		t.Fatalf("logged %d times with the statistics disabled", logged)
	}
}

func TestLifecycleHookMissingIsLogged(t *testing.T) {
	logged := 0
	old := logNoLifecycleHook
	logNoLifecycleHook = func() { logged++ }
	t.Cleanup(func() { logNoLifecycleHook = old })

	e := NewEvents(t.TempDir())
	e.checkLifecycleHook()
	e.checkLifecycleHook() // a reconnection
	if logged != 1 {
		t.Fatalf("logged %d times without a hook", logged)
	}

	logged = 0
	e = NewEvents(t.TempDir())
	e.OnLifecycle(func(vmstats.LifecycleEvent) {})
	e.checkLifecycleHook()
	if logged != 0 {
		t.Errorf("logged %d times with a hook", logged)
	}
}
