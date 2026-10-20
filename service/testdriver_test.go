package service

// These tests run the service against libvirt's test driver: a libvirtd that
// answers test:///default keeps domains, networks and pools in memory and
// starts no QEMU, so they need no KVM. They are skipped unless
// VIRT_TEST_LIBVIRT_URI is set (CI sets it to test:///default);
// VIRT_TEST_LIBVIRT_SOCKET is the daemon's socket, /run/libvirt/libvirt-sock
// when unset.

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/firewall"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
)

// iptablesCalls records what the firewall would have run.
type iptablesCalls struct {
	mu    sync.Mutex
	stdin []string
}

func (c *iptablesCalls) run(_ context.Context, stdin string, _ string, _ ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if stdin != "" {
		c.stdin = append(c.stdin, stdin)
	}

	return "", nil
}

func (c *iptablesCalls) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.stdin) == 0 {
		return ""
	}

	return c.stdin[len(c.stdin)-1]
}

// testDriverVMs connects to the test driver and returns the service on it,
// with the iptables calls it makes. The driver starts with one running domain
// called "test"; what a test creates, it deletes.
func testDriverVMs(t *testing.T) (*VMs, *iptablesCalls) {
	t.Helper()

	// no MessageBus: an event that cannot be published is only logged
	return testDriverVMsOn(t, testDriverConn(t), NewEvents(t.TempDir()))
}

// testDriverVMsOn is testDriverVMs on a given connection and events.
func testDriverVMsOn(t *testing.T, conn *libvirt.Conn, events *Events) (*VMs, *iptablesCalls) {
	t.Helper()

	oldPath := config.StorageInfo.Path
	config.StorageInfo.Path = t.TempDir()
	t.Cleanup(func() { config.StorageInfo.Path = oldPath })

	net, err := libvirt.ParseNetworkSpec("casaos", "casaos0", "192.168.123.0/24")
	if err != nil {
		t.Fatal(err)
	}

	// no iptables in a test: the rules are only recorded
	calls := &iptablesCalls{}
	fw := firewall.NewWithRunner(calls.run, filepath.Join(t.TempDir(), "ip_forward"))

	return NewVMs(t.Context(), conn, events, net, fw), calls
}

func TestDriverList(t *testing.T) {
	vms, _ := testDriverVMs(t)

	list, err := vms.List()
	if err != nil {
		t.Fatal(err)
	}

	if !slices.ContainsFunc(list, func(vm codegen.VM) bool { return vm.Name == "test" }) {
		t.Fatalf("the test driver's own domain is not listed: %+v", list)
	}
}

func TestDriverLifecycle(t *testing.T) {
	vms, _ := testDriverVMs(t)

	start := false
	vm, importing, err := vms.Create(codegen.VMCreate{Title: "CI machine", Vcpus: 1, MemoryMib: 256, DiskGib: 1, Start: &start})
	if err != nil {
		t.Fatal(err)
	}
	if importing {
		t.Fatal("a blank disk is not an import")
	}
	if vm.Title != "CI machine" || string(vm.State) != libvirt.StateShutOff {
		t.Fatalf("created %+v", vm)
	}

	id := vm.Id

	for _, step := range []struct {
		action codegen.VMAction
		state  string
	}{
		{codegen.Start, libvirt.StateRunning},
		{codegen.Pause, libvirt.StatePaused},
		{codegen.Resume, libvirt.StateRunning},
		{codegen.ForceOff, libvirt.StateShutOff},
	} {
		got, err := vms.ChangeState(id, step.action)
		if err != nil {
			t.Fatalf("%s: %v", step.action, err)
		}
		if string(got.State) != step.state {
			t.Fatalf("%s: the VM is %s, want %s", step.action, got.State, step.state)
		}
	}

	if _, err := vms.Delete(id, true); err != nil {
		t.Fatal(err)
	}

	if _, err := vms.Get(id); err == nil {
		t.Fatal("the VM is still there after delete")
	}
}

func TestDriverDeleteRunning(t *testing.T) {
	vms, _ := testDriverVMs(t)

	vm, _, err := vms.Create(codegen.VMCreate{Title: "running", Vcpus: 1, MemoryMib: 256, DiskGib: 1})
	if err != nil {
		t.Fatal(err)
	}
	id := vm.Id

	if _, err := vms.Delete(id, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete of a running VM: %v, want a conflict", err)
	}

	if _, err := vms.ChangeState(id, codegen.ForceOff); err != nil {
		t.Fatal(err)
	}
	if _, err := vms.Delete(id, true); err != nil {
		t.Fatal(err)
	}
}

func TestDriverUpdateAndPorts(t *testing.T) {
	vms, calls := testDriverVMs(t)

	start := false
	vm, _, err := vms.Create(codegen.VMCreate{Title: "ports", Vcpus: 1, MemoryMib: 256, DiskGib: 1, Start: &start})
	if err != nil {
		t.Fatal(err)
	}
	id := vm.Id
	t.Cleanup(func() { _, _ = vms.Delete(id, true) })

	if vm.Network == nil || !vm.Network.Casaos || vm.Network.Ip == nil || !strings.HasPrefix(*vm.Network.Ip, "192.168.123.") {
		t.Fatalf("a new VM is on the CasaOS network with a fixed address: %+v", vm.Network)
	}

	title, vcpus, memory := "ports, renamed", 2, 512
	vm, err = vms.Update(id, codegen.VMUpdate{Title: &title, Vcpus: &vcpus, MemoryMib: &memory})
	if err != nil {
		t.Fatal(err)
	}
	if vm.Title != title || vm.Vcpus == nil || *vm.Vcpus != vcpus || vm.MemoryMib == nil || *vm.MemoryMib != memory {
		t.Fatalf("updated %+v", vm)
	}

	vm, err = vms.SetPorts(id, []codegen.PortForward{{Protocol: codegen.Tcp, HostPort: 58222, GuestPort: 22}})
	if err != nil {
		t.Fatal(err)
	}
	if vm.Ports == nil || len(*vm.Ports) != 1 || (*vm.Ports)[0].HostPort != 58222 {
		t.Fatalf("ports %+v", vm.Ports)
	}

	rules := calls.last()
	if !strings.Contains(rules, "--dport 58222") || !strings.Contains(rules, *vm.Network.Ip+":22") {
		t.Fatalf("the forward is not in the rules applied:\n%s", rules)
	}

	// a host port is forwarded to one VM only
	other, _, err := vms.Create(codegen.VMCreate{Title: "other", Vcpus: 1, MemoryMib: 256, DiskGib: 1, Start: &start})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = vms.Delete(other.Id, true) })

	if _, err := vms.SetPorts(other.Id, []codegen.PortForward{{Protocol: codegen.Tcp, HostPort: 58222, GuestPort: 22}}); !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrConflict) {
		t.Fatalf("the same host port for a second VM: %v, want a refusal", err)
	}
}
