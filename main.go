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
	"sync"
	"syscall"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/model"
	"github.com/ReCasaOS/CasaOS-Common/pkg/stats"
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

	// register at message bus, before the statistics: the alerts they rebuild
	// from their history are published as soon as they open
	if err := events.Register(ctx); err != nil {
		logger.Error("error when trying to register one or more event types - some event type will not be discoverable", zap.Error(err))
	}

	// opened, and wired to the lifecycle events, before libvirt is dialled, so
	// no event is read without them
	st := openStats(config.StatsInfo.Path, events, conn)

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

	// every VM's CPU, memory, network and disk every 5 s, and their alerts.
	// While libvirt is away the list answers 503 and the sampler tries again
	// at each tick. Run closes the history when ctx ends; main waits for it,
	// so a stop loses at most the minute in progress.
	var sampling sync.WaitGroup
	if st != nil {
		sampling.Go(func() { st.Run(ctx) })
	}

	v2Router := route.InitV2Router(conn, vms, st)
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

	cancel()
	sampling.Wait()
}

// openStats opens the VM statistics in dir, nil when they cannot be: their
// routes then answer 503 and the rest of the service is unaffected.
func openStats(dir string, events *service.Events, conn *libvirt.Conn) *service.Stats {
	st, err := service.NewStats(service.StatsOptions{Dir: dir, Clock: stats.SystemClock{}, Source: service.NewConnSource(conn), Publisher: events})
	if err != nil {
		logger.Error("the VM statistics did not start, their routes answer 503", zap.String("path", dir), zap.Error(err))
		return nil
	}
	wireStats(events, st)
	return st
}

// wireStats hands the lifecycle events to the statistics: their crash alerts
// come from them.
func wireStats(events *service.Events, st *service.Stats) {
	events.OnLifecycle(st.HandleLifecycle)
}
