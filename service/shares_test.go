package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// shareRoots makes a /DATA and a /media under a temporary folder and points
// StorageRoots at them for the test.
func shareRoots(t *testing.T) (data, media, outside string) {
	t.Helper()

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, media, outside = filepath.Join(base, "DATA"), filepath.Join(base, "media"), filepath.Join(base, "etc")
	for _, d := range []string{filepath.Join(data, "AppData"), filepath.Join(media, "sda1", "data_cam"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(data, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(data, "escape")); err != nil {
		t.Fatal(err)
	}

	saved := StorageRoots
	StorageRoots = []string{data, media}
	t.Cleanup(func() { StorageRoots = saved })

	return data, media, outside
}

func TestValidateShareFolder(t *testing.T) {
	data, media, outside := shareRoots(t)

	cam := filepath.Join(media, "sda1", "data_cam")
	if got, err := ValidateShareFolder(cam + "/"); err != nil || got != cam {
		t.Fatalf("%q %v", got, err)
	}

	for _, bad := range []string{
		"relative/path",
		filepath.Join(data, "missing"),
		filepath.Join(data, "file"),
		filepath.Join(data, "escape"), // a link out of the roots
		outside,
		data, // a root itself: every app's data
		media,
	} {
		if _, err := ValidateShareFolder(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", bad, err)
		}
	}
}
