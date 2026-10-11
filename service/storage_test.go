package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateStorageFolder(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(base, "DATA")
	media := filepath.Join(base, "media")
	outside := filepath.Join(base, "etc")
	for _, d := range []string{filepath.Join(data, "VM"), filepath.Join(media, "Big", "VM"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(data, "file"), nil, 0o644)
	os.Symlink(outside, filepath.Join(data, "escape"))
	os.Symlink(filepath.Join(media, "Big"), filepath.Join(data, "big"))

	saved := StorageRoots
	StorageRoots = []string{data, media}
	defer func() { StorageRoots = saved }()

	for _, ok := range []string{filepath.Join(data, "VM"), filepath.Join(media, "Big", "VM"), filepath.Join(data, "VM") + "/"} {
		if _, err := ValidateStorageFolder(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}

	if got, err := ValidateStorageFolder(filepath.Join(data, "big")); err != nil || got != filepath.Join(media, "Big") {
		t.Errorf("a link into /media: %q %v", got, err)
	}

	for _, bad := range []string{"VM", outside, filepath.Join(data, "escape"), filepath.Join(data, "file"), filepath.Join(data, "missing"), filepath.Join(data, "..", "etc")} {
		if _, err := ValidateStorageFolder(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err %v", bad, err)
		}
	}
}
