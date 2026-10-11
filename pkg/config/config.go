package config

import (
	"path/filepath"

	"github.com/ReCasaOS/CasaOS-Common/utils/constants"
)

var (
	VirtManagementConfigFilePath = filepath.Join(constants.DefaultConfigPath, "virt-management.conf")

	// DefaultLibvirtSockets are tried in this order when [libvirt] Socket is
	// empty: the monolithic libvirtd (Debian, Ubuntu), then the modular
	// virtqemud most other distributions run.
	DefaultLibvirtSockets = []string{
		"/run/libvirt/libvirt-sock",
		"/run/libvirt/virtqemud-sock",
	}
)
