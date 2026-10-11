package service

import (
	"bufio"
	"context"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/firewall"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/hostnet"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// firewallInterval is how often the rules are written again, whatever
// happened: a firewall reload, or someone flushing the tables, loses them.
const firewallInterval = time.Minute

// PrepareNetwork makes sure the CasaOS network is there and running, and its
// rules written; on every libvirt (re)connection.
func (s *VMs) PrepareNetwork(_ context.Context, l *golibvirt.Libvirt) {
	if _, err := libvirt.EnsureNetwork(l, s.net); err != nil {
		logger.Error("error when preparing the VM network", zap.String("network", s.net.Name), zap.Error(err))
		return
	}

	s.applyFirewall(l)
}

// RunFirewall writes the rules now and then until ctx ends.
func (s *VMs) RunFirewall(ctx context.Context) {
	t := time.NewTicker(firewallInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if l, err := s.conn.Get(); err == nil {
				s.applyFirewall(l)
			}
		}
	}
}

// applyFirewall writes the rules for the forwards every VM has now.
func (s *VMs) applyFirewall(l *golibvirt.Libvirt) {
	s.fwMu.Lock()
	defer s.fwMu.Unlock()

	rules, err := s.firewallRules(l, uuid.Nil)
	if err != nil {
		logger.Error("error when reading the port forwards", zap.Error(err))
		return
	}

	if err := s.fw.Apply(s.ctx, rules); err != nil {
		logger.Error("error when writing the VM network's firewall rules", zap.Error(err))
	}
}

// casaosNetwork returns the CasaOS network and what its definition says.
func (s *VMs) casaosNetwork(l *golibvirt.Libvirt) (golibvirt.Network, libvirt.NetworkInfo, error) {
	nw, err := l.NetworkLookupByName(s.net.Name)
	if err != nil {
		return nw, libvirt.NetworkInfo{}, fmt.Errorf("network %s: %w", s.net.Name, err)
	}

	info, err := libvirt.NetworkInfoOf(l, nw)
	return nw, info, err
}

// onCasaOS reports whether d's first NIC is on the CasaOS network, and its MAC.
func (s *VMs) onCasaOS(d libvirt.Domain) (string, bool) {
	if len(d.NICs) == 0 || d.NICs[0].Type != "network" || d.NICs[0].Network != s.net.Name {
		return "", false
	}

	return d.NICs[0].MAC, true
}

// firewallRules collects the forwards of every VM on the CasaOS network, and
// the host bridges VMs are on, but for the VM with id except (uuid.Nil: none).
func (s *VMs) firewallRules(l *golibvirt.Libvirt, except uuid.UUID) (firewall.Rules, error) {
	rules := firewall.Rules{Bridge: s.net.Bridge, Subnet: s.net.Subnet}

	_, info, err := s.casaosNetwork(l)
	if err != nil {
		return rules, err
	}

	domains, _, err := l.ConnectListAllDomains(1, 0)
	if err != nil {
		return rules, err
	}

	for _, dom := range domains {
		if uuid.UUID(dom.UUID) == except {
			continue
		}

		desc, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
		if err != nil {
			continue // undefined meanwhile
		}

		d, err := libvirt.ParseDomainXML(desc)
		if err != nil || d.Metadata == nil {
			continue
		}

		// a running VM stays on its bridge until it restarts, whatever its
		// definition says now
		nics := d.NICs
		if live, err := l.DomainGetXMLDesc(dom, 0); err == nil {
			if ld, err := libvirt.ParseDomainXML(live); err == nil {
				nics = append(nics, ld.NICs...)
			}
		}
		for _, nic := range nics {
			if nic.Type == libvirt.NICBridge && !slices.Contains(rules.HostBridges, nic.Source) {
				rules.HostBridges = append(rules.HostBridges, nic.Source)
			}
		}

		mac, ok := s.onCasaOS(d)
		if !ok {
			continue
		}

		ip, err := netip.ParseAddr(info.IPOf(mac))
		if err != nil {
			continue // no fixed address: nothing to forward to
		}

		for _, p := range d.Metadata.Ports {
			rules.Forwards = append(rules.Forwards, firewall.Forward{Protocol: p.Protocol, HostPort: p.Host, IP: ip, GuestPort: p.Guest})
		}
	}

	return rules, nil
}

// nicFor is the NIC choice asks for, with mac: the CasaOS network when choice
// is nil, or a bridge or card the host has.
func (s *VMs) nicFor(choice *codegen.NetworkChoice, mac string) (libvirt.NIC, error) {
	if choice != nil && choice.Mode != codegen.NetworkChoiceModeCasaos && addressOf(choice) != "" {
		return libvirt.NIC{}, fmt.Errorf("%w: only VMs on the CasaOS network get their address from CasaOS", ErrInvalid)
	}

	if choice == nil || choice.Mode == codegen.NetworkChoiceModeCasaos {
		return libvirt.NIC{Type: libvirt.NICNetwork, Source: s.net.Name, MAC: mac}, nil
	}

	source := ""
	if choice.Source != nil {
		source = *choice.Source
	}

	devices, err := hostnet.Read(hostnet.SysClassNet, s.net.Bridge)
	if err != nil {
		return libvirt.NIC{}, fmt.Errorf("read the host's network devices: %w", err)
	}

	switch choice.Mode {
	case codegen.NetworkChoiceModeBridge:
		if !slices.Contains(devices.Bridges, source) {
			return libvirt.NIC{}, fmt.Errorf("%w: the host has no bridge %q a VM can use", ErrInvalid, source)
		}
		return libvirt.NIC{Type: libvirt.NICBridge, Source: source, MAC: mac}, nil
	case codegen.NetworkChoiceModeMacvtap:
		if !slices.Contains(devices.Links, source) {
			return libvirt.NIC{}, fmt.Errorf("%w: the host has no wired card %q free for macvtap", ErrInvalid, source)
		}
		return libvirt.NIC{Type: libvirt.NICDirect, Source: source, MAC: mac}, nil
	}

	return libvirt.NIC{}, fmt.Errorf("%w: unknown network mode %q", ErrInvalid, choice.Mode)
}

// addressOf is the address choice asks for; empty when it asks for none.
func addressOf(choice *codegen.NetworkChoice) string {
	if choice == nil || choice.Address == nil {
		return ""
	}
	return strings.TrimSpace(*choice.Address)
}

// arpAddress is the IPv4 address the host's ARP table has for mac, which a
// running VM on a bridge gets once it talks to the host or through it.
func arpAddress(l *golibvirt.Libvirt, dom golibvirt.Domain, mac string) string {
	return interfaceAddress(l, dom, mac, golibvirt.DomainInterfaceAddressesSrcArp)
}

// interfaceAddress is the IPv4 address libvirt knows for mac, from source.
func interfaceAddress(l *golibvirt.Libvirt, dom golibvirt.Domain, mac string, source golibvirt.DomainInterfaceAddressesSource) string {
	ifaces, err := l.DomainInterfaceAddresses(dom, uint32(source), 0)
	if err != nil {
		return ""
	}

	for _, i := range ifaces {
		if len(i.Hwaddr) == 0 || !strings.EqualFold(i.Hwaddr[0], mac) {
			continue
		}
		for _, a := range i.Addrs {
			if a.Type == int32(golibvirt.IPAddrTypeIpv4) {
				return a.Addr
			}
		}
	}

	return ""
}

// reserve gives mac a fixed address on the CasaOS network, or returns the one
// it has.
func (s *VMs) reserve(l *golibvirt.Libvirt, mac string) (string, error) {
	nw, info, err := s.casaosNetwork(l)
	if err != nil {
		return "", err
	}

	if ip := info.IPOf(mac); ip != "" {
		return ip, nil
	}

	ip, err := info.FreeFixedIP(s.net)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrConflict, err)
	}

	if err := libvirt.AddDHCPHost(l, nw, mac, ip.String()); err != nil {
		return "", fmt.Errorf("reserve an address: %w", err)
	}

	return ip.String(), nil
}

// AddressDHCP asks for an address from the CasaOS network's DHCP range.
const AddressDHCP = "dhcp"

// setAddress gives mac the address wanted on the CasaOS network: "" keeps
// the fixed address it has or picks a free one, AddressDHCP gives the fixed
// one back, and an address in the fixed range is reserved when it is free.
// It reports whether mac has a fixed address afterwards.
func (s *VMs) setAddress(l *golibvirt.Libvirt, mac, wanted string) (bool, error) {
	nw, info, err := s.casaosNetwork(l)
	if err != nil {
		return false, err
	}

	current := info.IPOf(mac)

	switch wanted {
	case "":
		if current != "" {
			return true, nil
		}
		_, err := s.reserve(l, mac)
		return err == nil, err
	case AddressDHCP:
		if current != "" {
			if err := libvirt.RemoveDHCPHost(l, nw, mac, current); err != nil {
				return true, fmt.Errorf("give the fixed address back: %w", err)
			}
		}
		return false, nil
	}

	ip, err := netip.ParseAddr(wanted)
	first, last := s.net.FixedRange()
	if err != nil || !ip.Is4() || ip.Less(first) || last.Less(ip) {
		return current != "", fmt.Errorf("%w: a fixed address goes from %s to %s", ErrInvalid, first, last)
	}

	if ip.String() == current {
		return true, nil
	}

	for _, h := range info.Hosts {
		if h.IP == ip.String() {
			return current != "", fmt.Errorf("%w: another VM has %s", ErrConflict, ip)
		}
	}

	if current != "" {
		if err := libvirt.RemoveDHCPHost(l, nw, mac, current); err != nil {
			return true, fmt.Errorf("give the old address back: %w", err)
		}
	}

	if err := libvirt.AddDHCPHost(l, nw, mac, ip.String()); err != nil {
		if current != "" {
			if err := libvirt.AddDHCPHost(l, nw, mac, current); err != nil {
				logger.Error("error when putting a VM's address back", zap.String("mac", mac), zap.String("ip", current), zap.Error(err))
			}
		}
		return current != "", fmt.Errorf("reserve %s: %w", ip, err)
	}

	return true, nil
}

// leaseAddress is the address a running VM on the CasaOS network got from
// its DHCP range.
func leaseAddress(l *golibvirt.Libvirt, dom golibvirt.Domain, mac string) string {
	return interfaceAddress(l, dom, mac, golibvirt.DomainInterfaceAddressesSrcLease)
}

// release takes mac's fixed address back, if it has one.
func (s *VMs) release(l *golibvirt.Libvirt, mac string) {
	nw, info, err := s.casaosNetwork(l)
	if err != nil {
		logger.Error("error when releasing a VM's address", zap.String("mac", mac), zap.Error(err))
		return
	}

	if ip := info.IPOf(mac); ip != "" {
		if err := libvirt.RemoveDHCPHost(l, nw, mac, ip); err != nil {
			logger.Error("error when releasing a VM's address", zap.String("mac", mac), zap.String("ip", ip), zap.Error(err))
		}
	}
}

// newMAC picks a MAC no reservation on the CasaOS network has.
func (s *VMs) newMAC(l *golibvirt.Libvirt) (string, error) {
	_, info, err := s.casaosNetwork(l)
	if err != nil {
		return "", err
	}

	used := make([]string, 0, len(info.Hosts))
	for _, h := range info.Hosts {
		used = append(used, h.MAC)
	}

	return libvirt.NewMAC(used)
}

// setMetadata writes CasaOS's metadata into the definition and, when the VM
// runs, into the live VM, which the API reads.
func setMetadata(l *golibvirt.Libvirt, dom golibvirt.Domain, m libvirt.Metadata, live bool) error {
	metadataXML, err := libvirt.BuildMetadataXML(m)
	if err != nil {
		return err
	}

	flags := golibvirt.DomainAffectConfig
	if live {
		flags |= golibvirt.DomainAffectLive
	}

	return l.DomainSetMetadata(dom, int32(golibvirt.DomainMetadataElement),
		golibvirt.OptString{metadataXML}, golibvirt.OptString{"casaos"}, golibvirt.OptString{libvirt.MetadataNamespace}, flags)
}

// SetPorts replaces a VM's port forwards and writes the rules at once.
func (s *VMs) SetPorts(id uuid.UUID, ports []codegen.PortForward) (codegen.VM, error) {
	l, dom, err := s.lookup(id)
	if err != nil {
		return codegen.VM{}, err
	}

	state, err := s.state(l, dom)
	if err != nil {
		return codegen.VM{}, err
	}
	if state == libvirt.StateCreating {
		return codegen.VM{}, fmt.Errorf("%w: the VM is still being created", ErrConflict)
	}

	desc, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive)
	if err != nil {
		return codegen.VM{}, err
	}

	d, err := libvirt.ParseDomainXML(desc)
	if err != nil {
		return codegen.VM{}, err
	}

	if d.Metadata == nil {
		return codegen.VM{}, fmt.Errorf("%w: CasaOS did not create this VM, so it does not forward ports to it", ErrConflict)
	}

	mac, ok := s.onCasaOS(d)
	if !ok {
		return codegen.VM{}, fmt.Errorf("%w: the VM is not on the %s network; move it there first", ErrConflict, s.net.Name)
	}

	if _, info, err := s.casaosNetwork(l); err != nil {
		return codegen.VM{}, err
	} else if len(ports) > 0 && info.IPOf(mac) == "" {
		return codegen.VM{}, fmt.Errorf("%w: the VM gets its address by DHCP, and forwards need a fixed one; give it one first", ErrConflict)
	}

	others, err := s.firewallRules(l, id)
	if err != nil {
		return codegen.VM{}, err
	}

	wanted, err := validatePorts(ports, others.Forwards, d.Metadata.Ports, listening)
	if err != nil {
		return codegen.VM{}, err
	}

	m := *d.Metadata
	m.Ports = wanted
	live := state != libvirt.StateShutOff && state != libvirt.StateCrashed
	if err := setMetadata(l, dom, m, live); err != nil {
		return codegen.VM{}, fmt.Errorf("record the port forwards: %w", err)
	}

	s.applyFirewall(l)

	logger.Info("set the port forwards", zap.String("vm", d.Name), zap.Any("ports", wanted))

	return s.toVM(l, dom)
}

// validatePorts checks the forwards asked for: a protocol, ports in range, no
// host port twice, none another VM forwards, and none something on the host
// listens on. A port the VM forwards already is its own, and stays free.
func validatePorts(ports []codegen.PortForward, others []firewall.Forward, current []libvirt.Port, isListening func(protocol string, port int) bool) ([]libvirt.Port, error) {
	type key struct {
		protocol string
		port     int
	}

	seen := map[key]bool{}
	for _, o := range others {
		seen[key{o.Protocol, o.HostPort}] = true
	}
	taken := func(k key) bool { return seen[k] }
	mine := func(k key) bool {
		return slices.ContainsFunc(current, func(p libvirt.Port) bool { return p.Protocol == k.protocol && p.Host == k.port })
	}

	out := make([]libvirt.Port, 0, len(ports))
	asked := map[key]bool{}
	for _, p := range ports {
		protocol := string(p.Protocol)
		if protocol != "tcp" && protocol != "udp" {
			return nil, fmt.Errorf("%w: protocol %q is neither tcp nor udp", ErrInvalid, protocol)
		}
		if p.HostPort < 1 || p.HostPort > 65535 || p.GuestPort < 1 || p.GuestPort > 65535 {
			return nil, fmt.Errorf("%w: ports go from 1 to 65535", ErrInvalid)
		}

		k := key{protocol, p.HostPort}
		switch {
		case asked[k]:
			return nil, fmt.Errorf("%w: host port %s/%d is forwarded twice", ErrInvalid, protocol, p.HostPort)
		case taken(k):
			return nil, fmt.Errorf("%w: another VM forwards host port %s/%d", ErrConflict, protocol, p.HostPort)
		case !mine(k) && isListening(protocol, p.HostPort):
			return nil, fmt.Errorf("%w: something on this machine already uses port %s/%d", ErrConflict, protocol, p.HostPort)
		}
		asked[k] = true

		out = append(out, libvirt.Port{Protocol: protocol, Host: p.HostPort, Guest: p.GuestPort})
	}

	return out, nil
}

// listening reports whether a socket of the host is bound to port: a TCP
// listener, or any UDP socket, on IPv4 or IPv6. Docker's published ports have
// docker-proxy listening on them, so they count.
func listening(protocol string, port int) bool {
	files := []string{"/proc/net/" + protocol, "/proc/net/" + protocol + "6"}
	for _, f := range files {
		if portInProcNet(f, protocol, port) {
			return true
		}
	}

	return false
}

func portInProcNet(path, protocol string, port int) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	return portInProcNetLines(bufio.NewScanner(f), protocol, port)
}

// portInProcNetLines reads /proc/net/{tcp,udp}{,6}: "sl local_address
// rem_address st ...", addresses as HEX_IP:HEX_PORT, st 0A for a TCP listener.
func portInProcNetLines(sc *bufio.Scanner, protocol string, port int) bool {
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}

		i := strings.LastIndexByte(fields[1], ':')
		if i < 0 {
			continue
		}

		p, err := strconv.ParseUint(fields[1][i+1:], 16, 16)
		if err != nil || int(p) != port {
			continue
		}

		if protocol == "tcp" && fields[3] != "0A" {
			continue
		}

		return true
	}

	return false
}
