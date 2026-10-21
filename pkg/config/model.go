package config

type CommonModel struct {
	RuntimePath string
}

type APPModel struct {
	LogPath     string
	LogSaveName string
	LogFileExt  string
}

type LibvirtModel struct {
	// Socket is libvirtd's unix socket. Empty tries the monolithic daemon's
	// socket, then the modular one's (DefaultLibvirtSockets).
	Socket string
	// URI is the connection URI asked of the daemon on that socket. Empty is
	// qemu:///system. The tests point it at libvirt's test driver,
	// test:///default, which needs no KVM.
	URI string
}

// NetworkModel is the CasaOS network VMs are attached to.
type NetworkModel struct {
	Name   string
	Bridge string
	// Subnet is an IPv4 /24: the gateway is .1, fixed addresses .2-.99,
	// DHCP from .100.
	Subnet string
}

// StatsModel is where the VM statistics and their alert settings are kept.
type StatsModel struct {
	Path string
}

// ImagesModel bounds what the browser can upload into the iso folder.
type ImagesModel struct {
	// MaxUploadGiB is the largest upload; 0 or less is the default, 64.
	MaxUploadGiB int
}

type StorageModel struct {
	// Path holds the VM disks and the iso folder. It must be under /DATA, so
	// the file browser shows it.
	Path string
}
