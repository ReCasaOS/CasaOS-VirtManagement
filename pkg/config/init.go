package config

import (
	"fmt"
	"log"
	"os"

	"github.com/ReCasaOS/CasaOS-Common/utils/constants"
	"github.com/ReCasaOS/CasaOS-VirtManagement/common"
	"gopkg.in/ini.v1"
)

var (
	CommonInfo = &CommonModel{
		RuntimePath: constants.DefaultRuntimePath,
	}

	AppInfo = &APPModel{
		LogPath:     constants.DefaultLogPath,
		LogSaveName: common.VirtManagementServiceName,
		LogFileExt:  "log",
	}

	LibvirtInfo = &LibvirtModel{}

	NetworkInfo = &NetworkModel{
		Name:   "casaos",
		Bridge: "casaos0",
		Subnet: "192.168.123.0/24",
	}

	StorageInfo = &StorageModel{
		Path: "/DATA/VM",
	}

	ImagesInfo = &ImagesModel{
		MaxUploadGiB: DefaultMaxUploadGiB,
	}

	StatsInfo = &StatsModel{
		Path: "/var/lib/casaos/stats/virt_management",
	}

	Cfg            *ini.File
	ConfigFilePath string
)

func InitSetup(config string, sample string) {
	ConfigFilePath = VirtManagementConfigFilePath
	if len(config) > 0 {
		ConfigFilePath = config
	}

	// create default config file if not exist
	if _, err := os.Stat(ConfigFilePath); os.IsNotExist(err) {
		fmt.Println("config file not exist, create it")

		if err := os.WriteFile(ConfigFilePath, []byte(sample), 0o644); err != nil {
			panic(err)
		}
	}

	var err error

	Cfg, err = ini.LoadSources(ini.LoadOptions{Insensitive: true, AllowShadows: true}, ConfigFilePath)
	if err != nil {
		panic(err)
	}

	mapTo("common", CommonInfo)
	mapTo("app", AppInfo)
	mapTo("libvirt", LibvirtInfo)
	mapTo("network", NetworkInfo)
	mapTo("storage", StorageInfo)
	mapTo("stats", StatsInfo)
	mapTo("images", ImagesInfo)
}

// DefaultMaxUploadGiB is [images] MaxUploadGiB when it is not set.
const DefaultMaxUploadGiB = 64

// MaxUploadBytes is the largest image the browser can upload, at most 1 PiB
// so that the byte count cannot overflow.
func MaxUploadBytes() int64 {
	if ImagesInfo.MaxUploadGiB <= 0 {
		return DefaultMaxUploadGiB << 30
	}
	return int64(min(ImagesInfo.MaxUploadGiB, 1<<20)) << 30
}

// LibvirtSockets is where libvirtd is looked for, in order.
func LibvirtSockets() []string {
	if LibvirtInfo.Socket != "" {
		return []string{LibvirtInfo.Socket}
	}

	return DefaultLibvirtSockets
}

func mapTo(section string, v interface{}) {
	err := Cfg.Section(section).MapTo(v)
	if err != nil {
		log.Fatalf("Cfg.MapTo %s err: %v", section, err)
	}
}
