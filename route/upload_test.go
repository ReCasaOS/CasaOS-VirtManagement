package route

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-VirtManagement/service"
	"github.com/labstack/echo/v4"
)

// TestUploadImageRoute sends 2 MiB through the whole router, over TCP, as the
// gateway would: authentication, the validator it skips, the generated
// binder, the handler and the disk.
func TestUploadImageRoute(t *testing.T) {
	logger.LogInitConsoleOnly()
	runtime := t.TempDir()
	previousRuntime := config.CommonInfo.RuntimePath
	config.CommonInfo.RuntimePath = runtime
	t.Cleanup(func() { config.CommonInfo.RuntimePath = previousRuntime })
	if err := os.WriteFile(filepath.Join(runtime, external.InternalSecretFilename), []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	previousRoot := service.DataRoot
	service.DataRoot = root
	t.Cleanup(func() { service.DataRoot = previousRoot })
	previousStorage := config.StorageInfo.Path
	config.StorageInfo.Path = filepath.Join(root, "VM")
	t.Cleanup(func() { config.StorageInfo.Path = previousStorage })
	dir := filepath.Join(root, "VM", service.ISOFolderName)

	vms := &service.VMs{Uploads: service.NewUploads(func() string { return dir }, 64<<30)}
	srv := httptest.NewServer(InitV2Router(nil, vms, nil))
	defer srv.Close()

	body := make([]byte, 2<<20)
	rand.Read(body)
	copy(body[32769:], "CD001")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])

	put := func(name, auth string, body []byte) (int, map[string]json.RawMessage) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v2/virt_management/images/upload?name="+name+"&sha256="+sha, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/octet-stream")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]json.RawMessage
		json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	if status, _ := put("debian.iso", "", body); status != http.StatusUnauthorized {
		t.Errorf("no token: %d", status)
	}

	status, out := put("debian.iso", "Internal s3cret", body)
	if status != http.StatusCreated {
		t.Fatalf("%d %s", status, out["message"])
	}
	var image struct {
		Path, Name, Kind, Sha256 string
		SizeBytes                int64 `json:"size_bytes"`
		Warning                  *string
	}
	if err := json.Unmarshal(out["data"], &image); err != nil {
		t.Fatal(err)
	}
	if image.Path != filepath.Join(dir, "debian.iso") || image.Name != "debian.iso" || image.Kind != "iso" ||
		image.SizeBytes != int64(len(body)) || image.Sha256 != sha || image.Warning != nil {
		t.Errorf("image %+v", image)
	}
	if got, _ := os.ReadFile(image.Path); !bytes.Equal(got, body) {
		t.Error("the file differs from what was sent")
	}

	if status, out := put("debian.iso", "Internal s3cret", body); status != http.StatusConflict || string(out["message"]) != `"exists"` {
		t.Errorf("again: %d %s", status, out["message"])
	}
	if status, out := put("..%2Fx.iso", "Internal s3cret", body); status != http.StatusBadRequest || string(out["message"]) != `"bad_name"` {
		t.Errorf("hostile name: %d %s", status, out["message"])
	}

	// the list shows it at once
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/virt_management/images", nil)
	req.Header.Set("Authorization", "Internal s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	listed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(listed), `"name":"debian.iso"`) || strings.Contains(string(listed), ".part") {
		t.Errorf("listed %s", listed)
	}
}

func TestIsImageUpload(t *testing.T) {
	e := echo.New()
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPut, V2APIPath + "/images/upload", true},
		{http.MethodGet, V2APIPath + "/images", false},
		{http.MethodPost, V2APIPath + "/images/downloads", false},
		{http.MethodPut, V2APIPath + "/stats/:id/alerts", false},
	} {
		c := e.NewContext(httptest.NewRequest(tc.method, "/", nil), httptest.NewRecorder())
		c.SetPath(tc.path)
		if got := isImageUpload(c); got != tc.want {
			t.Errorf("%s %s: %v", tc.method, tc.path, got)
		}
	}
}
