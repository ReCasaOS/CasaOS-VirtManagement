package v2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/labstack/echo/v4"
)

func ptr[T any](v T) *T { return &v }

// rolesRuntime is a runtime folder whose user-service says account 1 is the administrator and
// any other a user; with answers false it says nothing at all.
func rolesRuntime(t *testing.T, answers bool) {
	t.Helper()
	runtime := t.TempDir()
	previous := config.CommonInfo.RuntimePath
	config.CommonInfo.RuntimePath = runtime
	t.Cleanup(func() { config.CommonInfo.RuntimePath = previous })

	userService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !answers {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		role := "user"
		if strings.HasSuffix(r.URL.Path, "/role/1") {
			role = "admin"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": 200, "message": "ok", "data": role, "rights": []string{"vm"}})
	}))
	t.Cleanup(userService.Close)
	for name, text := range map[string]string{external.InternalSecretFilename: "s3cret", external.UserServiceAddressFilename: userService.URL} {
		if err := os.WriteFile(filepath.Join(runtime, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func caller(userID string, internal bool) echo.Context {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "203.0.113.5:40000"
	if internal {
		request.RemoteAddr = "127.0.0.1:40000"
		request.Header.Set("Authorization", "Internal s3cret")
	}
	if userID != "" {
		request.Header.Set("user_id", userID)
	}

	return echo.New().NewContext(request, httptest.NewRecorder())
}

func TestOnlyTheAdministratorAndTheBoxReadTheHostPaths(t *testing.T) {
	rolesRuntime(t, true)

	for name, c := range map[string]struct {
		ctx          echo.Context
		owner, known bool
	}{
		"the administrator":  {caller("1", false), true, true},
		"a service":          {caller("", true), true, true},
		"a user":             {caller("2", false), false, true},
		"no account at all":  {caller("", false), false, false},
		"an id not a number": {caller("x", false), false, false},
	} {
		owner, known := readsHostPaths(c.ctx)
		if owner != c.owner || known != c.known {
			t.Errorf("%s: owner %v known %v, want %v %v", name, owner, known, c.owner, c.known)
		}
	}
}

// A role that cannot be read is not answered with the paths left out: the administrator would
// read a VM without them.
func TestARoleThatCannotBeReadIsNotKnown(t *testing.T) {
	rolesRuntime(t, false)

	// accounts no other test asked about: the roles are kept a while
	for _, id := range []string{"41", "42"} {
		if owner, known := readsHostPaths(caller(id, false)); owner || known {
			t.Errorf("account %s: owner %v known %v, want neither", id, owner, known)
		}
	}
	if owner, known := readsHostPaths(caller("", true)); !owner || !known {
		t.Errorf("a service needs no user-service: owner %v known %v", owner, known)
	}

	recorder := httptest.NewRecorder()
	if err := roleUnknown(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), recorder)); err != nil || recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("code %d, err %v", recorder.Code, err)
	}
}

func TestAVMWithoutItsHostPaths(t *testing.T) {
	vm := codegen.VM{
		Name:   "1a2b3c4d",
		Title:  "web",
		Cdrom:  ptr("/DATA/VM/iso/debian.iso"),
		Disks:  &[]codegen.Disk{{Path: "/DATA/VM/1a2b3c4d.qcow2", Target: "vda", Managed: true}, {Path: "/media/sda1/data.qcow2", Target: "vdb"}},
		Shares: &[]codegen.Share{{Source: "/media/sda1/data_cam", Tag: "cam"}},
	}

	got := withoutHostPaths(vm)

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/DATA", "/media", ".iso", ".qcow2"} {
		if strings.Contains(string(body), path) {
			t.Errorf("%s is still in %s", path, body)
		}
	}
	// what is not a place on the host stays, and each disk and share is still listed
	if got.Title != "web" || len(*got.Disks) != 2 || (*got.Disks)[0].Target != "vda" || !(*got.Disks)[0].Managed || (*got.Shares)[0].Tag != "cam" {
		t.Errorf("more than the paths is gone: %s", body)
	}
	if !strings.Contains(string(body), `"path":""`) || !strings.Contains(string(body), `"source":""`) {
		t.Errorf("the keys the contract requires are gone: %s", body)
	}
	// the VM given keeps its own
	if *vm.Cdrom != "/DATA/VM/iso/debian.iso" || (*vm.Disks)[0].Path != "/DATA/VM/1a2b3c4d.qcow2" || (*vm.Shares)[0].Source != "/media/sda1/data_cam" {
		t.Errorf("the VM given was changed: %+v", vm)
	}

	// nothing to leave out: nothing added
	if bare := withoutHostPaths(codegen.VM{}); bare.Cdrom != nil || bare.Disks != nil || bare.Shares != nil {
		t.Errorf("a VM with nothing got %+v", bare)
	}
}

func TestTheHostWithoutItsPaths(t *testing.T) {
	host := codegen.Host{StoragePath: "/DATA/VM", Network: "casaos", Reason: ptr("dial unix /var/run/libvirt/libvirt-sock: no such file")}

	got := hostWithoutPaths(host)

	if got.StoragePath != "" || strings.Contains(*got.Reason, "/") || *got.Reason == "" || got.Network != "casaos" {
		t.Errorf("got %+v, reason %q", got, *got.Reason)
	}
	if *host.Reason == *got.Reason {
		t.Error("the host given was changed")
	}
	if got := hostWithoutPaths(codegen.Host{StoragePath: "/DATA/VM"}); got.Reason != nil {
		t.Errorf("a reason appeared: %q", *got.Reason)
	}
}
