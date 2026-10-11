package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewDiskPath(t *testing.T) {
	dir := t.TempDir()

	got, err := newDiskPath(dir, "1a2b3c4d")
	if err != nil || got != filepath.Join(dir, "1a2b3c4d-1.qcow2") {
		t.Fatalf("got %q, %v", got, err)
	}

	// a file left by a removed disk is skipped, never reused
	for _, name := range []string{"1a2b3c4d-1.qcow2", "1a2b3c4d-2.qcow2"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err = newDiskPath(dir, "1a2b3c4d")
	if err != nil || got != filepath.Join(dir, "1a2b3c4d-3.qcow2") {
		t.Fatalf("got %q, %v", got, err)
	}
}
