package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyImageName(t *testing.T) {
	cases := []struct {
		name        string
		iso         bool
		format      string
		compression string
		ok          bool
	}{
		{"alpine.ISO", true, "", "", true},
		{"haos_ova-16.2.qcow2.xz", false, "qcow2", "xz", true},
		{"disk.img", false, "raw", "", true},
		{"disk.img.gz", false, "raw", "gz", true},
		{"win.vmdk", false, "vmdk", "", true},
		{"vbox.vdi", false, "vdi", "", true},
		{"bundle.zip", false, "", "zip", true},
		{"notes.txt", false, "", "", false},
		{"archive.tar.gz", false, "", "", false},
		{"iso.xz", false, "", "", false},
	}

	for _, tc := range cases {
		iso, format, compression, ok := ClassifyImageName(tc.name)
		if iso != tc.iso || format != tc.format || compression != tc.compression || ok != tc.ok {
			t.Errorf("%s: got %v %q %q %v", tc.name, iso, format, compression, ok)
		}
	}
}

func TestValidateImagePath(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "DATA")
	outside := filepath.Join(base, "outside")

	for _, dir := range []string{filepath.Join(data, "VM", "iso"), outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	write := func(path string) {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(filepath.Join(data, "VM", "iso", "alpine.iso"))
	write(filepath.Join(data, "notes.txt"))
	write(filepath.Join(outside, "secret.iso"))

	if err := os.Symlink(filepath.Join(outside, "secret.iso"), filepath.Join(data, "link.iso")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(data, "VM", "iso", "alpine.iso"), filepath.Join(data, "inside.iso")); err != nil {
		t.Fatal(err)
	}

	old := DataRoot
	DataRoot = data
	t.Cleanup(func() { DataRoot = old })

	valid := []string{
		filepath.Join(data, "VM", "iso", "alpine.iso"),
		filepath.Join(data, "inside.iso"),
		filepath.Join(data, "VM", "iso", "..", "iso", "alpine.iso"),
	}
	for _, path := range valid {
		img, err := ValidateImagePath(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if !img.ISO || filepath.Base(img.Path) != "alpine.iso" {
			t.Errorf("%s: got %+v", path, img)
		}
	}

	invalid := []string{
		"VM/iso/alpine.iso", // relative
		filepath.Join(data, "..", "outside", "secret.iso"), // escapes with ..
		filepath.Join(data, "link.iso"),                    // symlink out of /DATA
		filepath.Join(data, "notes.txt"),                   // not an image
		filepath.Join(data, "VM", "iso"),                   // a directory
		filepath.Join(data, "missing.iso"),                 // missing
		data,                                               // the root itself
	}
	for _, path := range invalid {
		if _, err := ValidateImagePath(path); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", path, err)
		}
	}
}

func TestListImages(t *testing.T) {
	storage := t.TempDir()
	dir := filepath.Join(storage, ISOFolderName)
	if err := os.MkdirAll(filepath.Join(dir, "sub.iso"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b.iso", "A.qcow2.xz", "readme.txt", ".hidden.iso"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("xx"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	images, err := ListImages(storage)
	if err != nil {
		t.Fatal(err)
	}

	if len(images) != 2 || images[0].Name != "A.qcow2.xz" || images[0].Kind != "disk" || images[1].Name != "b.iso" || images[1].Kind != "iso" {
		t.Errorf("got %+v", images)
	}

	empty, err := ListImages(filepath.Join(storage, "missing"))
	if err != nil || len(empty) != 0 {
		t.Errorf("missing folder: got %v, %v", empty, err)
	}
}
