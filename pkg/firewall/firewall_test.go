package firewall

import (
	"context"
	"errors"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func rules() Rules {
	return Rules{
		Bridge: "casaos0",
		Subnet: netip.MustParsePrefix("192.168.123.0/24"),
		Forwards: []Forward{
			{"udp", 5353, netip.MustParseAddr("192.168.123.3"), 53},
			{"tcp", 8081, netip.MustParseAddr("192.168.123.2"), 80},
			{"tcp", 2222, netip.MustParseAddr("192.168.123.2"), 22},
		},
		HostBridges: []string{"br1", "br0"},
	}
}

func TestPayload(t *testing.T) {
	got := Payload(rules())

	path := filepath.Join("testdata", "payload.txt")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("payload differs from the golden file; go test -update rewrites it\n%s", got)
	}
}

// fakeIptables answers like iptables: a chain exists or not, a rule is there
// or not; it records what it was asked to change.
type fakeIptables struct {
	dockerUser bool
	present    map[string]bool // "table chain rule..." already in place
	changes    []string
}

func (f *fakeIptables) run(_ context.Context, _ string, name string, args ...string) (string, error) {
	line := strings.Join(args, " ")
	switch {
	case name == "iptables-restore":
		f.changes = append(f.changes, "restore")
		return "", nil
	case strings.HasPrefix(line, "-t filter -S DOCKER-USER"):
		if f.dockerUser {
			return "-N DOCKER-USER\n", nil
		}
		return "", errors.New("no chain")
	case strings.Contains(line, " -C "):
		if f.present[strings.Replace(line, " -C ", " ", 1)] {
			return "", nil
		}
		return "", errors.New("no rule")
	case strings.Contains(line, " -I "):
		f.changes = append(f.changes, line)
		return "", nil
	}
	return "", errors.New("unexpected " + line)
}

func newFake(t *testing.T, fake *fakeIptables, ipForward string) *Firewall {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ip_forward")
	if err := os.WriteFile(path, []byte(ipForward), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Firewall{run: fake.run, ipForward: path}
}

func TestApplyInsertsMissingJumpsOnly(t *testing.T) {
	fake := &fakeIptables{dockerUser: true, present: map[string]bool{
		"-t nat POSTROUTING -j CASAOS-VM-POST": true,
	}}
	fw := newFake(t, fake, "0\n")

	if err := fw.Apply(context.Background(), rules()); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"restore",
		"-t filter -I DOCKER-USER 1 -j CASAOS-VM",
		"-t nat -I PREROUTING 1 -m addrtype --dst-type LOCAL -j CASAOS-VM-DNAT",
		"-t nat -I OUTPUT 1 ! -d 127.0.0.0/8 -m addrtype --dst-type LOCAL -j CASAOS-VM-DNAT",
	}
	if strings.Join(fake.changes, "\n") != strings.Join(want, "\n") {
		t.Errorf("changes:\n%s", strings.Join(fake.changes, "\n"))
	}

	if b, _ := os.ReadFile(fw.ipForward); strings.TrimSpace(string(b)) != "1" {
		t.Errorf("ip_forward = %q", b)
	}
}

func TestApplyWithoutDocker(t *testing.T) {
	fake := &fakeIptables{present: map[string]bool{}}
	fw := newFake(t, fake, "1\n")

	if err := fw.Apply(context.Background(), rules()); err != nil {
		t.Fatal(err)
	}
	if fake.changes[1] != "-t filter -I FORWARD 1 -j CASAOS-VM" {
		t.Errorf("filter jump: %s", fake.changes[1])
	}
}
