package libvirt

import (
	"crypto/rand"
	"encoding/xml"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// NetworkSpec is the network CasaOS attaches VMs to: a libvirt network in
// forward mode 'open', where libvirt writes no firewall rules and the service
// writes its own (pkg/firewall). Its subnet is a /24: the gateway is .1, VMs
// get fixed addresses from .2 to .99, and anything else DHCP from .100 up.
type NetworkSpec struct {
	Name   string
	Bridge string
	Subnet netip.Prefix
}

// ParseNetworkSpec checks the configured network.
func ParseNetworkSpec(name, bridge, subnet string) (NetworkSpec, error) {
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil {
		return NetworkSpec{}, fmt.Errorf("network subnet %q: %w", subnet, err)
	}

	if !prefix.Addr().Is4() || prefix.Bits() != 24 {
		return NetworkSpec{}, fmt.Errorf("network subnet %q: an IPv4 /24 is needed", subnet)
	}

	// the kernel limits interface names to 15 characters
	if name == "" || bridge == "" || len(bridge) > 15 {
		return NetworkSpec{}, fmt.Errorf("network name %q or bridge %q is not valid", name, bridge)
	}

	return NetworkSpec{Name: name, Bridge: bridge, Subnet: prefix.Masked()}, nil
}

// host returns the address .n of the subnet.
func (s NetworkSpec) host(n byte) netip.Addr {
	a := s.Subnet.Addr().As4()
	a[3] = n
	return netip.AddrFrom4(a)
}

func (s NetworkSpec) Gateway() netip.Addr { return s.host(1) }

// FixedRange is where VMs get their fixed addresses.
func (s NetworkSpec) FixedRange() (first, last netip.Addr) { return s.host(2), s.host(99) }

type networkXML struct {
	XMLName xml.Name `xml:"network"`
	Name    string   `xml:"name"`
	Forward struct {
		Mode string `xml:"mode,attr"`
	} `xml:"forward"`
	Bridge struct {
		Name  string `xml:"name,attr"`
		STP   string `xml:"stp,attr,omitempty"`
		Delay string `xml:"delay,attr,omitempty"`
	} `xml:"bridge"`
	IPs []networkIPXML `xml:"ip"`
}

type networkIPXML struct {
	Address string   `xml:"address,attr"`
	Netmask string   `xml:"netmask,attr,omitempty"`
	Prefix  string   `xml:"prefix,attr,omitempty"`
	DHCP    *dhcpXML `xml:"dhcp"`
}

type dhcpXML struct {
	Range *dhcpRangeXML `xml:"range"`
	Hosts []DHCPHost    `xml:"host"`
}

type dhcpRangeXML struct {
	Start string `xml:"start,attr"`
	End   string `xml:"end,attr"`
}

// DHCPHost is a fixed address the network's DHCP server gives a MAC.
type DHCPHost struct {
	XMLName xml.Name `xml:"host"`
	MAC     string   `xml:"mac,attr"`
	IP      string   `xml:"ip,attr"`
}

// BuildNetworkXML writes the definition of the CasaOS network.
func BuildNetworkXML(spec NetworkSpec) (string, error) {
	n := networkXML{Name: spec.Name}
	n.Forward.Mode = "open"
	n.Bridge.Name = spec.Bridge
	n.Bridge.STP = "on"
	n.Bridge.Delay = "0"

	n.IPs = []networkIPXML{{
		Address: spec.Gateway().String(),
		Netmask: "255.255.255.0",
		DHCP:    &dhcpXML{Range: &dhcpRangeXML{Start: spec.host(100).String(), End: spec.host(254).String()}},
	}}

	out, err := xml.MarshalIndent(n, "", "  ")
	if err != nil {
		return "", err
	}

	return string(out), nil
}

// NetworkInfo is what the service reads back from a network's XML.
type NetworkInfo struct {
	Forward string
	Bridge  string
	Hosts   []DHCPHost
}

// IPOf returns the fixed address of mac, if it has one.
func (n NetworkInfo) IPOf(mac string) string {
	for _, h := range n.Hosts {
		if strings.EqualFold(h.MAC, mac) {
			return h.IP
		}
	}

	return ""
}

// ParseNetworkXML reads a network's XML.
func ParseNetworkXML(s string) (NetworkInfo, error) {
	var n networkXML
	if err := xml.Unmarshal([]byte(s), &n); err != nil {
		return NetworkInfo{}, err
	}

	info := NetworkInfo{Forward: n.Forward.Mode, Bridge: n.Bridge.Name}
	for _, ip := range n.IPs {
		if ip.DHCP != nil {
			info.Hosts = append(info.Hosts, ip.DHCP.Hosts...)
		}
	}

	return info, nil
}

// FreeFixedIP returns the lowest fixed address no host has.
func (n NetworkInfo) FreeFixedIP(spec NetworkSpec) (netip.Addr, error) {
	taken := map[netip.Addr]bool{}
	for _, h := range n.Hosts {
		if a, err := netip.ParseAddr(h.IP); err == nil {
			taken[a] = true
		}
	}

	first, last := spec.FixedRange()
	for a := first; a.Compare(last) <= 0; a = a.Next() {
		if !taken[a] {
			return a, nil
		}
	}

	return netip.Addr{}, errors.New("the VM network has no fixed address left")
}

// NewMAC returns a random MAC in QEMU's range (52:54:00), not in used.
func NewMAC(used []string) (string, error) {
	for range 10 {
		b := make([]byte, 3)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}

		mac := fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[0], b[1], b[2])
		clash := false
		for _, u := range used {
			if strings.EqualFold(u, mac) {
				clash = true
			}
		}
		if !clash {
			return mac, nil
		}
	}

	return "", errors.New("could not find a free MAC address")
}

// EnsureNetwork returns the CasaOS network, defining, starting and
// autostarting it as needed. A network of that name that is not ours (another
// forward mode or bridge) is an error: the service does not rewrite it.
func EnsureNetwork(l *golibvirt.Libvirt, spec NetworkSpec) (golibvirt.Network, error) {
	nw, err := l.NetworkLookupByName(spec.Name)
	switch {
	case err == nil:
		desc, err := definitionXML(l, nw)
		if err != nil {
			return nw, err
		}

		info, err := ParseNetworkXML(desc)
		if err != nil {
			return nw, err
		}

		if info.Forward != "open" || info.Bridge != spec.Bridge {
			return nw, fmt.Errorf("network %s exists with forward mode %q and bridge %q, not open and %s", spec.Name, info.Forward, info.Bridge, spec.Bridge)
		}

	case isNotFoundNetwork(err):
		desc, err := BuildNetworkXML(spec)
		if err != nil {
			return nw, err
		}

		if nw, err = l.NetworkDefineXML(desc); err != nil {
			return nw, fmt.Errorf("define network: %w", err)
		}

	default:
		return nw, err
	}

	active, err := l.NetworkIsActive(nw)
	if err != nil {
		return nw, err
	}

	if active == 0 {
		if err := l.NetworkCreate(nw); err != nil {
			return nw, fmt.Errorf("start network: %w", err)
		}
	}

	if err := l.NetworkSetAutostart(nw, 1); err != nil {
		return nw, fmt.Errorf("autostart network: %w", err)
	}

	return nw, nil
}

// definitionXML is the network's definition. libvirt's test driver refuses the
// inactive flag; there the live XML stands in, which for the CasaOS network
// is the same, since every change goes to both.
func definitionXML(l *golibvirt.Libvirt, nw golibvirt.Network) (string, error) {
	desc, err := l.NetworkGetXMLDesc(nw, uint32(golibvirt.NetworkXMLInactive))
	if IsUnsupportedFlags(err) {
		return l.NetworkGetXMLDesc(nw, 0)
	}

	return desc, err
}

// NetworkInfoOf reads the definition of a network, where DHCP hosts live.
func NetworkInfoOf(l *golibvirt.Libvirt, nw golibvirt.Network) (NetworkInfo, error) {
	desc, err := definitionXML(l, nw)
	if err != nil {
		return NetworkInfo{}, err
	}

	return ParseNetworkXML(desc)
}

// AddDHCPHost gives mac the fixed address ip, in the running network and in
// its definition.
func AddDHCPHost(l *golibvirt.Libvirt, nw golibvirt.Network, mac, ip string) error {
	return updateDHCPHost(l, nw, golibvirt.NetworkUpdateCommandAddLast, mac, ip)
}

// RemoveDHCPHost takes mac's fixed address back.
func RemoveDHCPHost(l *golibvirt.Libvirt, nw golibvirt.Network, mac, ip string) error {
	return updateDHCPHost(l, nw, golibvirt.NetworkUpdateCommandDelete, mac, ip)
}

func updateDHCPHost(l *golibvirt.Libvirt, nw golibvirt.Network, cmd golibvirt.NetworkUpdateCommand, mac, ip string) error {
	out, err := xml.Marshal(DHCPHost{MAC: mac, IP: ip})
	if err != nil {
		return err
	}

	flags := golibvirt.NetworkUpdateAffectConfig
	if active, err := l.NetworkIsActive(nw); err == nil && active != 0 {
		flags |= golibvirt.NetworkUpdateAffectLive
	}

	return l.NetworkUpdate(nw, uint32(cmd), uint32(golibvirt.NetworkSectionIPDhcpHost), -1, string(out), flags)
}

func isNotFoundNetwork(err error) bool {
	var lerr golibvirt.Error
	return errors.As(err, &lerr) && golibvirt.ErrorNumber(lerr.Code) == golibvirt.ErrNoNetwork
}
