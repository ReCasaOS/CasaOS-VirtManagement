package hostnet

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRead(t *testing.T) {
	sys := t.TempDir()

	// name → entries under /sys/class/net/<name>
	devices := map[string][]string{
		"lo":              nil,
		"br0":             {"bridge/"},
		"casaos0":         {"bridge/"},
		"virbr0":          {"bridge/"},
		"docker0":         {"bridge/"},
		"br-1a2b3c":       {"bridge/"},
		"enp3s0":          {"device"},
		"enx00e04c447098": {"device", "master"}, // port of br0
		"wlp0s20f3":       {"device", "phy80211", "wireless/"},
		"vnet0":           {"master"},
		"veth12ab":        {"master"},
		"tailscale0":      nil, // virtual, no device
	}
	for name, entries := range devices {
		dir := filepath.Join(sys, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			var err error
			if e[len(e)-1] == '/' {
				err = os.Mkdir(filepath.Join(dir, e), 0o755)
			} else {
				err = os.WriteFile(filepath.Join(dir, e), nil, 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	d, err := Read(sys, "casaos0")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.Bridges, []string{"br0"}) {
		t.Errorf("bridges %v, want [br0]", d.Bridges)
	}
	if !slices.Equal(d.Links, []string{"enp3s0"}) {
		t.Errorf("links %v, want [enp3s0]", d.Links)
	}
}
