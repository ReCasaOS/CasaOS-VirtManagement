package config

import (
	"path/filepath"
	"sync"
)

var storageMu sync.RWMutex

// StoragePath is the folder new VMs keep their disks in, and whose iso
// folder holds the images: [storage] Path, which the dashboard can change.
func StoragePath() string {
	storageMu.RLock()
	defer storageMu.RUnlock()

	return filepath.Clean(StorageInfo.Path)
}

// SetStoragePath changes the folder and writes it to the config file, so a
// restart keeps it.
func SetStoragePath(path string) error {
	storageMu.Lock()
	defer storageMu.Unlock()

	if Cfg != nil {
		Cfg.Section("storage").Key("Path").SetValue(path)
		if err := Cfg.SaveTo(ConfigFilePath); err != nil {
			return err
		}
	}

	StorageInfo.Path = path
	return nil
}
