package v2

import (
	"errors"
	"net/http"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	"github.com/ReCasaOS/CasaOS-VirtManagement/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

type VirtManagement struct {
	conn *libvirt.Conn
	vms  *service.VMs
}

func NewVirtManagement(conn *libvirt.Conn, vms *service.VMs) codegen.ServerInterface {
	return &VirtManagement{conn: conn, vms: vms}
}

// fail answers err with the status its kind calls for.
func fail(ctx echo.Context, err error) error {
	status := http.StatusInternalServerError

	switch {
	case errors.Is(err, service.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, service.ErrNotFound), errors.Is(err, service.ErrDiskNotFound):
		status = http.StatusNotFound
	case errors.Is(err, service.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, libvirt.ErrUnavailable):
		status = http.StatusServiceUnavailable
	default:
		logger.Error("error when handling a request", zap.String("path", ctx.Path()), zap.Error(err))
	}

	message := err.Error()
	return ctx.JSON(status, codegen.BaseResponse{Message: &message})
}

func (v *VirtManagement) GetHost(ctx echo.Context) error {
	host := service.Host(v.conn, v.vms.Network())

	return ctx.JSON(http.StatusOK, codegen.HostOK{Data: &host})
}

func (v *VirtManagement) ListImages(ctx echo.Context) error {
	images, err := service.ListImages(config.StoragePath())
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.ImageListOK{Data: &images})
}

func (v *VirtManagement) ListVMs(ctx echo.Context) error {
	vms, err := v.vms.List()
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMListOK{Data: &vms})
}

func (v *VirtManagement) CreateVM(ctx echo.Context) error {
	var req codegen.VMCreate
	if err := ctx.Bind(&req); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.BaseResponse{Message: &message})
	}

	vm, importing, err := v.vms.Create(req)
	if err != nil {
		return fail(ctx, err)
	}

	status := http.StatusCreated
	if importing {
		status = http.StatusAccepted
	}

	return ctx.JSON(status, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) GetVM(ctx echo.Context, id codegen.VMID) error {
	vm, err := v.vms.Get(id)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) UpdateVM(ctx echo.Context, id codegen.VMID) error {
	var req codegen.VMUpdate
	if err := ctx.Bind(&req); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.BaseResponse{Message: &message})
	}

	vm, err := v.vms.Update(id, req)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) DeleteVM(ctx echo.Context, id codegen.VMID, params codegen.DeleteVMParams) error {
	result, err := v.vms.Delete(id, params.DeleteDisks != nil && *params.DeleteDisks)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMDeleteOK{Data: &result})
}

func (v *VirtManagement) ChangeVMState(ctx echo.Context, id codegen.VMID, action codegen.VMAction) error {
	vm, err := v.vms.ChangeState(id, action)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) SetCDROM(ctx echo.Context, id codegen.VMID) error {
	var req codegen.SetCDROMJSONRequestBody
	if err := ctx.Bind(&req); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.BaseResponse{Message: &message})
	}

	vm, err := v.vms.SetCDROM(id, req.IsoPath)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) AddDisk(ctx echo.Context, id codegen.VMID) error {
	var req codegen.AddDiskJSONRequestBody
	if err := ctx.Bind(&req); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.BaseResponse{Message: &message})
	}

	vm, err := v.vms.AddDisk(id, req.SizeGib)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) ResizeDisk(ctx echo.Context, id codegen.VMID, target codegen.DiskTarget) error {
	var req codegen.ResizeDiskJSONRequestBody
	if err := ctx.Bind(&req); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.BaseResponse{Message: &message})
	}

	vm, err := v.vms.ResizeDisk(id, target, req.SizeGib)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) RemoveDisk(ctx echo.Context, id codegen.VMID, target codegen.DiskTarget, params codegen.RemoveDiskParams) error {
	vm, err := v.vms.RemoveDisk(id, target, params.DeleteFile != nil && *params.DeleteFile)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) SetPorts(ctx echo.Context, id codegen.VMID) error {
	var req codegen.SetPortsJSONRequestBody
	if err := ctx.Bind(&req); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.BaseResponse{Message: &message})
	}

	vm, err := v.vms.SetPorts(id, req.Ports)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) CreateSnapshot(ctx echo.Context, id codegen.VMID) error {
	var req codegen.CreateSnapshotJSONRequestBody
	if err := ctx.Bind(&req); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.BaseResponse{Message: &message})
	}

	vm, err := v.vms.CreateSnapshot(id, req.Name)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) RevertSnapshot(ctx echo.Context, id codegen.VMID, snapshot codegen.SnapshotID) error {
	vm, err := v.vms.RevertSnapshot(id, snapshot)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) DeleteSnapshot(ctx echo.Context, id codegen.VMID, snapshot codegen.SnapshotID) error {
	vm, err := v.vms.DeleteSnapshot(id, snapshot)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) ListHostUSB(ctx echo.Context) error {
	devices, err := v.vms.HostUSB()
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.HostUSBOK{Data: &devices})
}

func (v *VirtManagement) SetUSB(ctx echo.Context, id codegen.VMID) error {
	var req codegen.SetUSBJSONRequestBody
	if err := ctx.Bind(&req); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.BaseResponse{Message: &message})
	}

	vm, err := v.vms.SetUSB(id, req.Devices)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.VMOK{Data: &vm})
}

func (v *VirtManagement) ListDownloads(ctx echo.Context) error {
	downloads := v.vms.Downloads.List()
	return ctx.JSON(http.StatusOK, codegen.DownloadListOK{Data: &downloads})
}

func (v *VirtManagement) StartDownload(ctx echo.Context) error {
	var req codegen.StartDownloadJSONRequestBody
	if err := ctx.Bind(&req); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.BaseResponse{Message: &message})
	}

	sha := ""
	if req.Sha256 != nil {
		sha = *req.Sha256
	}

	download, err := v.vms.Downloads.Start(req.Url, sha)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusAccepted, codegen.DownloadOK{Data: &download})
}

func (v *VirtManagement) DeleteDownload(ctx echo.Context, id string) error {
	download, err := v.vms.Downloads.Delete(id)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.DownloadOK{Data: &download})
}

func (v *VirtManagement) GetStorageFolder(ctx echo.Context, params codegen.GetStorageFolderParams) error {
	folder, err := service.StorageFolder(params.Path)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.StorageFolderOK{Data: &folder})
}

func (v *VirtManagement) SetStorage(ctx echo.Context) error {
	var req codegen.SetStorageJSONRequestBody
	if err := ctx.Bind(&req); err != nil {
		message := err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.BaseResponse{Message: &message})
	}

	folder, err := v.vms.SetDefaultStorage(req.Path)
	if err != nil {
		return fail(ctx, err)
	}

	return ctx.JSON(http.StatusOK, codegen.StorageFolderOK{Data: &folder})
}
