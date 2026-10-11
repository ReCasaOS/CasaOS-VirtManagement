// Package firewall writes the rules of the CasaOS VM network: libvirt writes
// none for a network in forward mode 'open', so the service does. It owns
// three chains and the jumps to them, and touches nothing else:
//
//	filter CASAOS-VM       from DOCKER-USER (or FORWARD, without Docker)
//	nat    CASAOS-VM-POST  from POSTROUTING: masquerade outward, and hairpin
//	nat    CASAOS-VM-DNAT  from PREROUTING and OUTPUT: the port forwards
//
// Measured on a test box with Ubuntu 26.04 and libvirt 12.
package firewall

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strings"
)

const (
	FilterChain = "CASAOS-VM"
	PostChain   = "CASAOS-VM-POST"
	DNATChain   = "CASAOS-VM-DNAT"
)

// Forward sends the host's port to a VM's.
type Forward struct {
	Protocol  string // tcp or udp
	HostPort  int
	IP        netip.Addr
	GuestPort int
}

// Rules is what the chains say.
type Rules struct {
	Bridge   string
	Subnet   netip.Prefix
	Forwards []Forward
	// HostBridges are the host's own bridges VMs are on. Traffic across one
	// is switched, not routed, but with br_netfilter loaded it goes through
	// FORWARD, where Docker's DROP policy would stop it.
	HostBridges []string
}

// Payload is the input of iptables-restore --noflush: declaring a chain
// creates or empties it, and the rules that follow fill it; other chains are
// left as they are.
func Payload(r Rules) string {
	var b strings.Builder
	sub := r.Subnet.String()

	b.WriteString("*filter\n")
	fmt.Fprintf(&b, ":%s - [0:0]\n", FilterChain)
	bridges := append([]string(nil), r.HostBridges...)
	sort.Strings(bridges)
	for _, br := range bridges {
		fmt.Fprintf(&b, "-A %s -i %s -o %s -j ACCEPT\n", FilterChain, br, br)
	}
	fmt.Fprintf(&b, "-A %s -o %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n", FilterChain, r.Bridge)
	fmt.Fprintf(&b, "-A %s -i %s -j ACCEPT\n", FilterChain, r.Bridge)
	fmt.Fprintf(&b, "-A %s -o %s -m conntrack --ctstate DNAT -j ACCEPT\n", FilterChain, r.Bridge)
	// containers reach the VMs at their own addresses
	fmt.Fprintf(&b, "-A %s -i br-+ -o %s -j ACCEPT\n", FilterChain, r.Bridge)
	fmt.Fprintf(&b, "-A %s -i docker0 -o %s -j ACCEPT\n", FilterChain, r.Bridge)
	fmt.Fprintf(&b, "-A %s -o %s -j REJECT --reject-with icmp-port-unreachable\n", FilterChain, r.Bridge)
	b.WriteString("COMMIT\n")

	b.WriteString("*nat\n")
	fmt.Fprintf(&b, ":%s - [0:0]\n", PostChain)
	fmt.Fprintf(&b, ":%s - [0:0]\n", DNATChain)
	fmt.Fprintf(&b, "-A %s -s %s -d 224.0.0.0/24 -j RETURN\n", PostChain, sub)
	fmt.Fprintf(&b, "-A %s -s %s -d 255.255.255.255/32 -j RETURN\n", PostChain, sub)
	fmt.Fprintf(&b, "-A %s -s %s ! -d %s -j MASQUERADE\n", PostChain, sub, sub)
	// a VM reaching a forward on the host's address comes back to the subnet
	fmt.Fprintf(&b, "-A %s -s %s -d %s -m conntrack --ctstate DNAT -j MASQUERADE\n", PostChain, sub, sub)

	forwards := append([]Forward(nil), r.Forwards...)
	sort.Slice(forwards, func(i, j int) bool {
		if forwards[i].Protocol != forwards[j].Protocol {
			return forwards[i].Protocol < forwards[j].Protocol
		}
		return forwards[i].HostPort < forwards[j].HostPort
	})
	for _, f := range forwards {
		fmt.Fprintf(&b, "-A %s -p %s -m %s --dport %d -j DNAT --to-destination %s:%d\n", DNATChain, f.Protocol, f.Protocol, f.HostPort, f.IP, f.GuestPort)
	}
	b.WriteString("COMMIT\n")

	return b.String()
}

// jump is a rule in a chain the service does not own, sending packets to one
// it does.
type jump struct {
	table string
	chain string
	rule  []string
}

func jumps(filterParent string) []jump {
	return []jump{
		{"filter", filterParent, []string{"-j", FilterChain}},
		{"nat", "POSTROUTING", []string{"-j", PostChain}},
		{"nat", "PREROUTING", []string{"-m", "addrtype", "--dst-type", "LOCAL", "-j", DNATChain}},
		{"nat", "OUTPUT", []string{"!", "-d", "127.0.0.0/8", "-m", "addrtype", "--dst-type", "LOCAL", "-j", DNATChain}},
	}
}

// Runner runs a command, with stdin; tests replace it.
type Runner func(ctx context.Context, stdin string, name string, args ...string) (string, error)

func execRunner(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// Firewall applies Rules.
type Firewall struct {
	run Runner
	// ipForward is the sysctl the rules need; a variable for the tests
	ipForward string
}

func New() *Firewall {
	return &Firewall{run: execRunner, ipForward: "/proc/sys/net/ipv4/ip_forward"}
}

// NewWithRunner runs every iptables call through run and enables forwarding
// in the file ipForward: the tests use it to apply rules without iptables.
func NewWithRunner(run Runner, ipForward string) *Firewall {
	return &Firewall{run: run, ipForward: ipForward}
}

// Apply rewrites the chains and puts back any jump that is missing. Run it as
// often as needed: at start, after a change, and now and then, since a
// firewall reload or a reboot loses the rules.
func (f *Firewall) Apply(ctx context.Context, r Rules) error {
	if _, err := f.run(ctx, Payload(r), "iptables-restore", "--noflush"); err != nil {
		return err
	}

	// Docker jumps to DOCKER-USER first and promises to leave it alone; without
	// Docker there is no such chain, and FORWARD takes the jump
	parent := "FORWARD"
	if _, err := f.run(ctx, "", "iptables", "-t", "filter", "-S", "DOCKER-USER"); err == nil {
		parent = "DOCKER-USER"
	}

	for _, j := range jumps(parent) {
		check := append([]string{"-t", j.table, "-C", j.chain}, j.rule...)
		if _, err := f.run(ctx, "", "iptables", check...); err == nil {
			continue
		}

		insert := append([]string{"-t", j.table, "-I", j.chain, "1"}, j.rule...)
		if _, err := f.run(ctx, "", "iptables", insert...); err != nil {
			return err
		}
	}

	// libvirt and Docker both turn it on; nothing is routed without it
	if b, err := os.ReadFile(f.ipForward); err == nil && strings.TrimSpace(string(b)) != "1" {
		if err := os.WriteFile(f.ipForward, []byte("1\n"), 0o644); err != nil {
			return fmt.Errorf("enable IPv4 forwarding: %w", err)
		}
	}

	return nil
}
