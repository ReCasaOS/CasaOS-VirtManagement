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

	// a box upgraded from a version without statistics keeps its config file,
	// which has no [stats]: the built-in default applies
	upgraded := filepath.Join(t.TempDir(), "upgraded.conf")
	if err := os.WriteFile(upgraded, []byte("[libvirt]\nURI =\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	InitSetup(upgraded, "")
	if StatsInfo.Path != "/var/lib/casaos/stats/virt_management" {
		t.Errorf("without [stats]: %q", StatsInfo.Path)
	}

	// a new box gets the sample: its [stats] Path is the default
	InitSetup(filepath.Join(t.TempDir(), "new.conf"), string(sample))
	if StatsInfo.Path != "/var/lib/casaos/stats/virt_management" {
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

func TestMaxUploadBytes(t *testing.T) {
	sample, err := os.ReadFile("../../build/sysroot/etc/casaos/virt-management.conf.sample")
	if err != nil {
		t.Fatal(err)
	}
	old := *ImagesInfo
	t.Cleanup(func() { *ImagesInfo = old })

	for _, tc := range []struct {
		conf string
		want int64
	}{
		{"[libvirt]\nURI =\n", 64 << 30}, // upgraded: no [images]
		{string(sample), 64 << 30},
		{"[images]\nMaxUploadGiB = 8\n", 8 << 30},
		{"[images]\nMaxUploadGiB = 0\n", 64 << 30},
	} {
		*ImagesInfo = old
		conf := filepath.Join(t.TempDir(), "virt.conf")
		if err := os.WriteFile(conf, []byte(tc.conf), 0o644); err != nil {
			t.Fatal(err)
		}
		InitSetup(conf, "")
		if got := MaxUploadBytes(); got != tc.want {
			t.Errorf("%q: %d, want %d", tc.conf, got, tc.want)
		}
	}
}
