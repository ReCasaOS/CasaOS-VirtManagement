package v2

import (
	"net/http"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/labstack/echo/v4"
)

// readsHostPaths says whether the caller reads where things are on the host: the box's own
// services and the administrator do, any other account does not. known is false when the role
// cannot be told (user-service does not answer): the caller is then answered 503, so an
// administrator is never given an answer with its paths left out.
func readsHostPaths(ctx echo.Context) (owner, known bool) {
	request := ctx.Request()
	if external.IsInternalRequest(ctx.RealIP(), request.Header.Get(echo.HeaderAuthorization), config.CommonInfo.RuntimePath) {
		return true, true
	}
	role, gone, known := external.Account(config.CommonInfo.RuntimePath, request.Header.Get("user_id"))

	return role == "admin" && !gone, known
}

// roleUnknown answers a caller whose role cannot be told.
func roleUnknown(ctx echo.Context) error {
	message := "the account's role cannot be read for now"

	return ctx.JSON(http.StatusServiceUnavailable, codegen.BaseResponse{Message: &message})
}

// withoutHostPaths is the VM as an account that is not the administrator reads it: the files of
// its disks, the ISO in its drive and the host folders it shares are there, without their place
// on the host. The VM given is not changed.
func withoutHostPaths(vm codegen.VM) codegen.VM {
	if vm.Cdrom != nil && *vm.Cdrom != "" {
		vm.Cdrom = new(string)
	}
	if vm.Disks != nil {
		disks := append([]codegen.Disk(nil), *vm.Disks...)
		for i := range disks {
			disks[i].Path = ""
		}
		vm.Disks = &disks
	}
	if vm.Shares != nil {
		shares := append([]codegen.Share(nil), *vm.Shares...)
		for i := range shares {
			shares[i].Source = ""
		}
		vm.Shares = &shares
	}

	return vm
}

// hostWithoutPaths is the host as an account that is not the administrator reads it: neither
// the folder of the VM disks nor libvirt's own words for why it does not answer.
func hostWithoutPaths(host codegen.Host) codegen.Host {
	host.StoragePath = ""
	if host.Reason != nil && *host.Reason != "" {
		reason := "libvirt does not answer"
		host.Reason = &reason
	}

	return host
}
