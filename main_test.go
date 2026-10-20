package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/vmstats"
	"github.com/ReCasaOS/CasaOS-VirtManagement/service"
	golibvirt "github.com/digitalocean/go-libvirt"
)

// The crash alerts come from the lifecycle events: the statistics main opens
// must receive them, or vm_crashed would silently fire from the sampler only.
func TestOpenStatsReceivesLifecycleEvents(t *testing.T) {
	logger.LogInitConsoleOnly()
	dir := filepath.Join(t.TempDir(), "stats")
	events := service.NewEvents(t.TempDir())
	st := openStats(dir, events, libvirt.New(nil, ""))
	if st == nil {
		t.Fatal("not opened")
	}

	events.HandleLifecycle(vmstats.LifecycleEvent{UUID: vmUUID, Name: "web", Event: golibvirt.DomainEventCrashed})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "crashed.json"))
	if err != nil || !strings.Contains(string(b), vmUUID) {
		t.Fatalf("crashed.json %s (%v): the crash did not reach the statistics", b, err)
	}
}

const vmUUID = "11111111-1111-1111-1111-111111111111"

// A statistics directory that cannot be made leaves the service without
// statistics (their routes answer 503), not without its other routes.
func TestOpenStatsFailureIsNotFatal(t *testing.T) {
	logger.LogInitConsoleOnly()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if st := openStats(filepath.Join(file, "stats"), service.NewEvents(t.TempDir()), libvirt.New(nil, "")); st != nil {
		t.Fatal("statistics opened under a regular file")
	}
	if st := openStats(filepath.Join(t.TempDir(), "stats"), service.NewEvents(t.TempDir()), libvirt.New(nil, "")); st == nil {
		t.Fatal("statistics not opened in a new directory")
	} else {
		_ = st.Close()
	}
}
