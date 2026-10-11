package libvirt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// PoolName is the libvirt storage pool holding the disks CasaOS creates.
const PoolName = "casaos-vm"

type poolXML struct {
	XMLName xml.Name `xml:"pool"`
	Type    string   `xml:"type,attr"`
	Name    string   `xml:"name"`
	Target  struct {
		Path        string          `xml:"path"`
		Permissions *permissionsXML `xml:"permissions"`
	} `xml:"target"`
}

type permissionsXML struct {
	Mode string `xml:"mode"`
}

// BuildPoolXML writes the definition of the dir pool name at path. The
// folder is 0755 like the other folders in /DATA (libvirt's default is 0711);
// the disks in it are 0600.
func BuildPoolXML(name, path string) (string, error) {
	p := poolXML{Type: "dir", Name: name}
	p.Target.Path = path
	p.Target.Permissions = &permissionsXML{Mode: "0755"}

	out, err := xml.Marshal(p)
	return string(out), err
}

// PoolNameFor names the pool of a folder that is not the first: casaos-vm-
// and 8 hex digits of the path, the same each time.
func PoolNameFor(path string) string {
	sum := sha256.Sum256([]byte(path))
	return PoolName + "-" + hex.EncodeToString(sum[:4])
}

type volumeXML struct {
	XMLName  xml.Name `xml:"volume"`
	Name     string   `xml:"name"`
	Capacity struct {
		Unit  string `xml:"unit,attr"`
		Value int64  `xml:",chardata"`
	} `xml:"capacity"`
	Target struct {
		Format struct {
			Type string `xml:"type,attr"`
		} `xml:"format"`
		// qcow2 v3: libvirt writes 0.10 otherwise, which cannot grow once
		// it has snapshots
		Compat string `xml:"compat,omitempty"`
	} `xml:"target"`
}

// BuildVolumeXML writes the definition of an empty qcow2 volume.
func BuildVolumeXML(name string, capacityBytes int64) (string, error) {
	v := volumeXML{Name: name}
	v.Capacity.Unit = "bytes"
	v.Capacity.Value = capacityBytes
	v.Target.Format.Type = "qcow2"
	v.Target.Compat = "1.1"

	out, err := xml.Marshal(v)
	return string(out), err
}

// EnsurePool returns the pool whose folder is path, defining, building
// (mkdir), starting and autostarting it as needed. Each folder VMs keep disks
// in has a pool: the first one made is casaos-vm, others casaos-vm-<hash>.
// A pool of CasaOS's name found pointing elsewhere is left alone.
func EnsurePool(l *golibvirt.Libvirt, path string) (golibvirt.StoragePool, error) {
	pool, found, err := poolAt(l, path)
	if err != nil {
		return pool, err
	}

	if !found {
		name := PoolName
		if _, err := l.StoragePoolLookupByName(PoolName); err == nil {
			name = PoolNameFor(path)
		} else if !IsNotFound(err) {
			return pool, err
		}

		desc, err := BuildPoolXML(name, path)
		if err != nil {
			return pool, err
		}

		if pool, err = l.StoragePoolDefineXML(desc, 0); err != nil {
			return pool, fmt.Errorf("define storage pool: %w", err)
		}

		if err := l.StoragePoolBuild(pool, golibvirt.StoragePoolBuildNew); err != nil {
			return pool, fmt.Errorf("build storage pool: %w", err)
		}
	}

	active, err := l.StoragePoolIsActive(pool)
	if err != nil {
		return pool, err
	}

	if active == 0 {
		if err := l.StoragePoolCreate(pool, 0); err != nil {
			return pool, fmt.Errorf("start storage pool: %w", err)
		}
	}

	if err := l.StoragePoolSetAutostart(pool, 1); err != nil {
		return pool, fmt.Errorf("autostart storage pool: %w", err)
	}

	return pool, nil
}

// poolAt finds the dir pool whose folder is path, whatever its name.
func poolAt(l *golibvirt.Libvirt, path string) (golibvirt.StoragePool, bool, error) {
	pools, _, err := l.ConnectListAllStoragePools(1, 0)
	if err != nil {
		return golibvirt.StoragePool{}, false, err
	}

	for _, pool := range pools {
		desc, err := l.StoragePoolGetXMLDesc(pool, 0)
		if err != nil {
			continue
		}

		var p poolXML
		if err := xml.Unmarshal([]byte(desc), &p); err != nil {
			continue
		}

		if p.Type == "dir" && filepath.Clean(p.Target.Path) == filepath.Clean(path) {
			return pool, true, nil
		}
	}

	return golibvirt.StoragePool{}, false, nil
}

// IsNotFound reports whether err is libvirt's "no such domain / pool / volume".
func IsNotFound(err error) bool {
	var lerr golibvirt.Error
	if !errors.As(err, &lerr) {
		return false
	}

	switch golibvirt.ErrorNumber(lerr.Code) {
	case golibvirt.ErrNoDomain, golibvirt.ErrNoStoragePool, golibvirt.ErrNoStorageVol:
		return true
	}

	return false
}

// IsUnsupportedFlags reports whether the driver refused a call's flags; the
// test driver lacks some that QEMU's has.
func IsUnsupportedFlags(err error) bool {
	var lerr golibvirt.Error
	if !errors.As(err, &lerr) {
		return false
	}

	return golibvirt.ErrorNumber(lerr.Code) == golibvirt.ErrInvalidArg && strings.Contains(lerr.Message, "unsupported flags")
}

// IsOperationInvalid reports whether libvirt refused err because of the
// domain's state (starting a running VM, and the like).
func IsOperationInvalid(err error) bool {
	var lerr golibvirt.Error
	if !errors.As(err, &lerr) {
		return false
	}

	return golibvirt.ErrorNumber(lerr.Code) == golibvirt.ErrOperationInvalid
}
