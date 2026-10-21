package service

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/hostnet"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// Host reports what this host can run. libvirt being unavailable is part of the
// answer, not an error.
func Host(conn *libvirt.Conn, net libvirt.NetworkSpec) codegen.Host {
	subnet, gateway := net.Subnet.String(), net.Gateway().String()
	host := codegen.Host{
		Network:        net.Name,
		NetworkSubnet:  &subnet,
		NetworkGateway: &gateway,
		Libvirt:        codegen.Unavailable,
		StoragePath:    config.StoragePath(),
	}

	if devices, err := hostnet.Read(hostnet.SysClassNet, net.Bridge); err != nil {
		logger.Error("error when listing the host's network devices", zap.Error(err))
	} else {
		bridges, links := append([]string{}, devices.Bridges...), append([]string{}, devices.Links...)
		host.Bridges, host.Links = &bridges, &links
	}

	if free, err := freeBytes(config.StoragePath()); err != nil {
		logger.Error("error when reading the free space of the VM storage", zap.Error(err), zap.String("path", config.StoragePath()))
	} else {
		host.StorageFreeBytes = &free
	}

	l, err := conn.Get()
	if err != nil {
		reason := err.Error()
		host.Reason = &reason
		return host
	}

	info, err := libvirt.GetHost(l)
	if err != nil {
		reason := err.Error()
		host.Reason = &reason
		return host
	}

	host.Libvirt = codegen.Available
	host.LibvirtVersion = &info.LibvirtVersion
	host.Cpus = &info.CPUs
	host.MemoryMib = &info.MemoryMiB
	host.Kvm = &info.KVM
	host.Uefi = &info.UEFI

	return host
}

// freeBytes is the space available to the service on the filesystem that holds
// path. The storage folder is created when the pool is, so until then the
// nearest existing parent is measured: it is the filesystem the folder will be
// created on.
func freeBytes(path string) (int64, error) {
	free, _, err := diskSpace(path)
	return free, err
}

// diskSpace is freeBytes and the size of that filesystem.
func diskSpace(path string) (free, total int64, err error) {
	for {
		var st unix.Statfs_t
		err := unix.Statfs(path, &st)
		if err == nil {
			return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), nil
		}

		parent := filepath.Dir(path)
		if !errors.Is(err, os.ErrNotExist) || parent == path {
			return 0, 0, err
		}
		path = parent
	}
}
