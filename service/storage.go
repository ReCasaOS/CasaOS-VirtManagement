package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	"go.uber.org/zap"
)

// StorageRoots are where VM disks may be: the data folder, and the disks
// ReCasaOS mounts under /media.
var StorageRoots = []string{"/DATA", "/media"}

// underStorageRoots reports whether path is inside one of StorageRoots.
func underStorageRoots(path string) bool {
	for _, root := range StorageRoots {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

// ValidateStorageFolder checks that path is an existing folder under
// StorageRoots, symlinks resolved, and returns it cleaned.
func ValidateStorageFolder(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: give an absolute path", ErrInvalid)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s does not exist", ErrInvalid, path)
		}
		return "", err
	}

	st, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%w: %s is not a folder", ErrInvalid, path)
	}

	if !underStorageRoots(resolved) {
		return "", fmt.Errorf("%w: VM disks go under %s", ErrInvalid, strings.Join(StorageRoots, " or "))
	}

	return resolved, nil
}

// StorageFolder describes a folder for VM disks.
func StorageFolder(path string) (codegen.StorageFolder, error) {
	folder, err := ValidateStorageFolder(path)
	if err != nil {
		return codegen.StorageFolder{}, err
	}

	out := codegen.StorageFolder{Path: folder, Default: folder == config.StoragePath()}
	if free, err := freeBytes(folder); err == nil {
		out.FreeBytes = &free
	}
	return out, nil
}

// SetDefaultStorage makes path the folder new VMs use, with its pool and its
// iso folder, and writes it to the config file.
func (s *VMs) SetDefaultStorage(path string) (codegen.StorageFolder, error) {
	folder, err := ValidateStorageFolder(path)
	if err != nil {
		return codegen.StorageFolder{}, err
	}

	l, err := s.conn.Get()
	if err != nil {
		return codegen.StorageFolder{}, err
	}

	if _, err := libvirt.EnsurePool(l, folder); err != nil {
		return codegen.StorageFolder{}, err
	}

	if err := os.MkdirAll(filepath.Join(folder, ISOFolderName), 0o755); err != nil {
		return codegen.StorageFolder{}, err
	}

	if err := config.SetStoragePath(folder); err != nil {
		return codegen.StorageFolder{}, fmt.Errorf("write the config file: %w", err)
	}

	logger.Info("changed the default folder for VM disks", zap.String("path", folder))

	return StorageFolder(folder)
}
