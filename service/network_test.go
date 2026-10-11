package service

import (
	"bufio"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/firewall"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
)

func TestValidatePorts(t *testing.T) {
	others := []firewall.Forward{{Protocol: "tcp", HostPort: 2222, IP: netip.MustParseAddr("192.168.123.3"), GuestPort: 22}}
	current := []libvirt.Port{{Protocol: "tcp", Host: 8081, Guest: 80}}
	// 8081 has no listener (a forward has none), 3000 is a container's
	isListening := func(protocol string, port int) bool { return protocol == "tcp" && port == 3000 }

	pf := func(p string, h, g int) codegen.PortForward {
		return codegen.PortForward{Protocol: codegen.PortForwardProtocol(p), HostPort: h, GuestPort: g}
	}

	got, err := validatePorts([]codegen.PortForward{pf("tcp", 8081, 8080), pf("udp", 3000, 53)}, others, current, isListening)
	if err != nil || len(got) != 2 || got[0] != (libvirt.Port{Protocol: "tcp", Host: 8081, Guest: 8080}) {
		t.Fatalf("got %+v, %v", got, err)
	}

	cases := map[string]struct {
		ports []codegen.PortForward
		want  error
	}{
		"bad protocol":       {[]codegen.PortForward{pf("icmp", 1, 1)}, ErrInvalid},
		"out of range":       {[]codegen.PortForward{pf("tcp", 70000, 1)}, ErrInvalid},
		"twice":              {[]codegen.PortForward{pf("tcp", 9000, 1), pf("tcp", 9000, 2)}, ErrInvalid},
		"another VM's":       {[]codegen.PortForward{pf("tcp", 2222, 22)}, ErrConflict},
		"a container's port": {[]codegen.PortForward{pf("tcp", 3000, 80)}, ErrConflict},
	}
	for name, tc := range cases {
		if _, err := validatePorts(tc.ports, others, current, isListening); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

// from a box: sshd on 22 listening, a connection from port 3000 established,
// docker-proxy listening on 3000 (0x0BB8)
const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 0100007F:0BB9 01 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 20 4 30 10 -1
   2: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 1 0000000000000000 100 0 0 10 0
`

func TestPortInProcNet(t *testing.T) {
	scan := func() *bufio.Scanner { return bufio.NewScanner(strings.NewReader(procNetTCP)) }

	if !portInProcNetLines(scan(), "tcp", 22) || !portInProcNetLines(scan(), "tcp", 3000) {
		t.Error("listeners on 22 and 3000 not found")
	}
	if portInProcNetLines(scan(), "tcp", 8080) {
		t.Error("an established connection is not a listener")
	}
	// UDP has no listening state: a bound socket counts
	if !portInProcNetLines(scan(), "udp", 8080) {
		t.Error("a bound UDP port counts")
	}
}
