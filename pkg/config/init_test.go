package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStatsPath(t *testing.T) {
	sample, err := os.ReadFile("../../build/sysroot/etc/casaos/virt-management.conf.sample")
	if err != nil {
		t.Fatal(err)
	}
	old := *StatsInfo
	t.Cleanup(func() { *StatsInfo = old })

	// a new box gets the sample: its [stats] Path is the default
	InitSetup(filepath.Join(t.TempDir(), "new.conf"), string(sample))
	if StatsInfo.Path != "/var/lib/casaos/stats/virt-management" {
		t.Errorf("from the sample: %q", StatsInfo.Path)
	}

	conf := filepath.Join(t.TempDir(), "set.conf")
	if err := os.WriteFile(conf, []byte("[stats]\nPath = /srv/stats\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	InitSetup(conf, "")
	if StatsInfo.Path != "/srv/stats" {
		t.Errorf("set: %q", StatsInfo.Path)
	}
}
