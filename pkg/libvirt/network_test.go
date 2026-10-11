package libvirt

import (
	"strconv"
	"strings"
	"testing"
)

func testSpec(t *testing.T) NetworkSpec {
	t.Helper()

	spec, err := ParseNetworkSpec("casaos", "casaos0", "192.168.123.0/24")
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestParseNetworkSpec(t *testing.T) {
	spec := testSpec(t)
	if spec.Gateway().String() != "192.168.123.1" {
		t.Errorf("gateway %s", spec.Gateway())
	}
	if first, last := spec.FixedRange(); first.String() != "192.168.123.2" || last.String() != "192.168.123.99" {
		t.Errorf("fixed range %s-%s", first, last)
	}

	for _, bad := range [][3]string{
		{"casaos", "casaos0", "192.168.123.0/16"},
		{"casaos", "casaos0", "fd00::/64"},
		{"casaos", "a-bridge-name-too-long", "192.168.123.0/24"},
		{"", "casaos0", "192.168.123.0/24"},
		{"casaos", "casaos0", "nonsense"},
	} {
		if _, err := ParseNetworkSpec(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("%v: expected an error", bad)
		}
	}
}

func TestBuildNetworkXML(t *testing.T) {
	got, err := BuildNetworkXML(testSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "network.xml", got)

	info, err := ParseNetworkXML(got)
	if err != nil {
		t.Fatal(err)
	}
	if info.Forward != "open" || info.Bridge != "casaos0" || len(info.Hosts) != 0 {
		t.Errorf("round trip: %+v", info)
	}
}

// the network as libvirt returns it with two reservations
const networkWithHosts = `<network>
  <name>casaos</name>
  <uuid>6f3c2d1e-0000-4000-8000-000000000000</uuid>
  <forward mode='open'/>
  <bridge name='casaos0' stp='on' delay='0'/>
  <mac address='52:54:00:aa:bb:cc'/>
  <ip address='192.168.123.1' netmask='255.255.255.0'>
    <dhcp>
      <range start='192.168.123.100' end='192.168.123.254'/>
      <host mac='52:54:00:00:00:01' ip='192.168.123.2'/>
      <host mac='52:54:00:00:00:03' ip='192.168.123.4'/>
    </dhcp>
  </ip>
</network>`

func TestNetworkHosts(t *testing.T) {
	info, err := ParseNetworkXML(networkWithHosts)
	if err != nil {
		t.Fatal(err)
	}

	if got := info.IPOf("52:54:00:00:00:03"); got != "192.168.123.4" {
		t.Errorf("IPOf = %q", got)
	}
	if got := info.IPOf("52:54:00:00:00:09"); got != "" {
		t.Errorf("IPOf of an unknown MAC = %q", got)
	}

	// the first gap, not the end
	free, err := info.FreeFixedIP(testSpec(t))
	if err != nil || free.String() != "192.168.123.3" {
		t.Errorf("FreeFixedIP = %v, %v", free, err)
	}

	full := NetworkInfo{}
	for n := 2; n <= 99; n++ {
		full.Hosts = append(full.Hosts, DHCPHost{IP: "192.168.123." + strconv.Itoa(n)})
	}
	if _, err := full.FreeFixedIP(testSpec(t)); err == nil {
		t.Error("expected an error with every fixed address taken")
	}
}

func TestNewMAC(t *testing.T) {
	mac, err := NewMAC(nil)
	if err != nil || !strings.HasPrefix(mac, "52:54:00:") || len(mac) != 17 {
		t.Errorf("NewMAC = %q, %v", mac, err)
	}
}

func TestInterfaceXMLAndNICs(t *testing.T) {
	cases := map[string]NIC{
		"interface.xml":         {Type: NICNetwork, Source: "casaos", MAC: "52:54:00:12:34:56"},
		"interface_bridge.xml":  {Type: NICBridge, Source: "br0", MAC: "52:54:00:12:34:57"},
		"interface_macvtap.xml": {Type: NICDirect, Source: "enp3s0", MAC: "52:54:00:12:34:58"},
	}

	for name, nic := range cases {
		got, err := BuildInterfaceXML(nic)
		if err != nil {
			t.Fatal(err)
		}
		golden(t, name, got)

		d, err := ParseDomainXML("<domain><devices>" + got + "</devices></domain>")
		if err != nil {
			t.Fatal(err)
		}
		want := Interface{Type: nic.Type, MAC: nic.MAC}
		if nic.Type == NICNetwork {
			want.Network = nic.Source
		} else {
			want.Source = nic.Source
		}
		if len(d.NICs) != 1 || d.NICs[0] != want {
			t.Errorf("%s: NICs %+v, want %+v", name, d.NICs, want)
		}
	}

	for _, bad := range []NIC{{Type: NICBridge}, {Type: "user", Source: "x"}} {
		if _, err := BuildInterfaceXML(bad); err == nil {
			t.Errorf("BuildInterfaceXML(%+v) should fail", bad)
		}
	}
}

func TestLiveNetworkNICHasNoSource(t *testing.T) {
	// libvirt adds the network's bridge to a running VM's NIC
	d, err := ParseDomainXML(`<domain><devices><interface type='network'><mac address='52:54:00:12:34:56'/><source network='casaos' portid='x' bridge='casaos0'/><target dev='vnet0'/></interface></devices></domain>`)
	if err != nil {
		t.Fatal(err)
	}
	if d.NICs[0] != (Interface{Type: NICNetwork, Network: "casaos", MAC: "52:54:00:12:34:56"}) {
		t.Errorf("NICs %+v", d.NICs)
	}
}

func TestMetadataPorts(t *testing.T) {
	m := Metadata{Volumes: []string{"/DATA/VM/a.qcow2"}, Firmware: FirmwareUEFI, Ports: []Port{{"tcp", 8081, 80}, {"udp", 5353, 53}}}

	got, err := BuildMetadataXML(m)
	if err != nil {
		t.Fatal(err)
	}

	stored := strings.NewReplacer("<vm>", `<casaos:vm xmlns:casaos="`+MetadataNamespace+`">`, "</vm>", "</casaos:vm>", "<port ", "<casaos:port ", "</port>", "</casaos:port>").Replace(got)
	d, err := ParseDomainXML("<domain><metadata>" + stored + "</metadata></domain>")
	if err != nil {
		t.Fatal(err)
	}
	if d.Metadata == nil || len(d.Metadata.Ports) != 2 || d.Metadata.Ports[1] != (Port{"udp", 5353, 53}) {
		t.Errorf("round trip: %+v", d.Metadata)
	}
}
