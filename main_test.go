package main

import (
	"go/format"
	"go/parser"
	"go/token"
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

// main cannot run in a test; this pins in its source, comments dropped, that
// it registers the events, then opens the statistics through openStats (which
// wires the lifecycle hook), dials libvirt, starts the sampler and serves it.
func TestMainWiresTheStatisticsInOrder(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0) // no comments
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := format.Node(&b, token.NewFileSet(), file); err != nil {
		t.Fatal(err)
	}
	src := b.String()
	body := src[strings.Index(src, "func main() {"):strings.Index(src, "\nfunc openStats(")]

	last, lastCall := -1, ""
	for _, call := range []string{
		"events.Register(ctx)",
		"openStats(config.StatsInfo.Path, events, conn)",
		"go conn.Run(ctx)",
		"sampling.Go(",
		"route.InitV2Router(conn, vms, st)",
	} {
		i := strings.Index(body, call)
		switch {
		case i < 0:
			t.Fatalf("main no longer calls %s", call)
		case i < last:
			t.Fatalf("main calls %s before %s", call, lastCall)
		}
		last, lastCall = i, call
	}
	if n := strings.Count(src, "service.NewStats("); n != 1 {
		t.Errorf("service.NewStats called %d times: main must open the statistics through openStats only", n)
	}
}
