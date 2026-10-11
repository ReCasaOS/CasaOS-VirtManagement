//go:generate bash -c "mkdir -p codegen && go run github.com/deepmap/oapi-codegen/cmd/oapi-codegen@v1.12.4 -generate types,server,spec -package codegen api/virt_management/openapi.yaml > codegen/virt_management_api.go"

package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/model"
	"github.com/ReCasaOS/CasaOS-Common/utils/file"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/common"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/firewall"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	"github.com/ReCasaOS/CasaOS-VirtManagement/route"
	"github.com/ReCasaOS/CasaOS-VirtManagement/service"
	"github.com/coreos/go-systemd/daemon"
	golibvirt "github.com/digitalocean/go-libvirt"
	"go.uber.org/zap"

	util_http "github.com/ReCasaOS/CasaOS-Common/utils/http"
)

var (
	commit = "private build"
	date   = "private build"

	//go:embed api/index.html
	_docHTML string

	//go:embed api/virt_management/openapi.yaml
	_docYAML string

	//go:embed build/sysroot/etc/casaos/virt-management.conf.sample
	_confSample string
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// parse arguments and intialize
	{
		configFlag := flag.String("c", "", "config file path")
		versionFlag := flag.Bool("v", false, "version")

		flag.Parse()

		if *versionFlag {
			fmt.Printf("v%s\n", common.VirtManagementVersion)
			os.Exit(0)
		}

		println("git commit:", commit)
		println("build date:", date)

		config.InitSetup(*configFlag, _confSample)

		logger.LogInit(config.AppInfo.LogPath, config.AppInfo.LogSaveName, config.AppInfo.LogFileExt)
	}

	// connect to libvirt in the background: the API is served whether libvirt
	// is there or not, and GET /host says which
	vmNet, err := libvirt.ParseNetworkSpec(config.NetworkInfo.Name, config.NetworkInfo.Bridge, config.NetworkInfo.Subnet)
	if err != nil {
		panic(err)
	}

	conn := libvirt.New(config.LibvirtSockets(), config.LibvirtInfo.URI)
	events := service.NewEvents(config.CommonInfo.RuntimePath)

	// on every (re)connection: make sure the storage pool is there, and follow
	// the VMs' lifecycle
	conn.OnConnect(func(_ context.Context, l *golibvirt.Libvirt) {
		if _, err := libvirt.EnsurePool(l, config.StoragePath()); err != nil {
			logger.Error("error when preparing the VM storage pool", zap.String("path", config.StoragePath()), zap.Error(err))
			return
		}

		if err := os.MkdirAll(filepath.Join(config.StoragePath(), service.ISOFolderName), 0o755); err != nil {
			logger.Error("error when creating the ISO folder", zap.Error(err))
		}
	})
	conn.OnConnect(events.WatchLifecycle)

	go conn.Run(ctx)

	vms := service.NewVMs(ctx, conn, events, vmNet, firewall.New())
	vms.Downloads.CleanParts()
	conn.OnConnect(vms.PrepareNetwork)
	go vms.RunFirewall(ctx)

	// register at message bus
	if err := events.Register(ctx); err != nil {
		logger.Error("error when trying to register one or more event types - some event type will not be discoverable", zap.Error(err))
	}

	// setup listener
	listener, err := net.Listen("tcp", net.JoinHostPort(common.Localhost, "0"))
	if err != nil {
		panic(err)
	}

	urlFilePath := filepath.Join(config.CommonInfo.RuntimePath, "virt-management.url")
	if err := file.CreateFileAndWriteContent(urlFilePath, "http://"+listener.Addr().String()); err != nil {
		logger.Error("error when creating address file", zap.Error(err),
			zap.Any("address", listener.Addr().String()),
			zap.Any("filepath", urlFilePath),
		)
	}

	// initialize routers and register at gateway
	{
		gateway, err := external.NewManagementService(config.CommonInfo.RuntimePath)
		if err != nil {
			panic(err)
		}

		apiPaths := []string{
			route.V2APIPath,
			route.V2DocPath,
		}

		for _, apiPath := range apiPaths {
			if err := gateway.CreateRoute(&model.Route{
				Path:   apiPath,
				Target: "http://" + listener.Addr().String(),
			}); err != nil {
				panic(err)
			}
		}
	}

	v2Router := route.InitV2Router(conn, vms)
	v2DocRouter := route.InitV2DocRouter(_docHTML, _docYAML)

	mux := &util_http.HandlerMultiplexer{
		HandlerMap: map[string]http.Handler{
			"v2":  v2Router,
			"doc": v2DocRouter,
		},
	}

	// notify systemd that we are ready
	{
		if supported, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
			logger.Error("Failed to notify systemd that virt management service is ready", zap.Any("error", err))
		} else if supported {
			logger.Info("Notified systemd that virt management service is ready")
		} else {
			logger.Info("This process is not running as a systemd service.")
		}

		logger.Info("Virt management service is listening...", zap.Any("address", listener.Addr().String()))
	}

	s := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // fix G112: Potential slowloris attack (see https://github.com/securego/gosec)
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := s.Shutdown(shutdownCtx); err != nil {
			logger.Error("error when shutting down the http server", zap.Error(err))
		}
	}()

	err = s.Serve(listener) // not using http.serve() to fix G114: Use of net/http serve function that has no support for setting timeouts (see https://github.com/securego/gosec)
	if err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}
