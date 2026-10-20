package service

// Probe of what libvirt's test driver answers to the calls the statistics
// sampler relies on. Skipped unless VIRT_TEST_LIBVIRT_URI is set.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
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
		for _, f := range []string{golibvirt.DomainStatsStateState, golibvirt.DomainStatsCPUTime} {
			if !fields[f] {
				t.Errorf("no %q for the domain test", f)
			}
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
