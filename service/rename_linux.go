//go:build linux

package service

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames from to to unless to exists (os.ErrExist). On a
// filesystem without RENAME_NOREPLACE (FUSE, ntfs-3g) it checks, then renames.
func renameNoReplace(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		return renameChecked(from, to)
	}
	if errors.Is(err, unix.EEXIST) {
		return os.ErrExist
	}
	return err
}
