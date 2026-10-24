package route

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils/jwt"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-VirtManagement/service"
)

// The access token bare (as the dashboard sends it) or as `Bearer <token>`; another scheme, or
// the scheme alone, is no token. Header or ?token= (console only): the raw header decides. Past
// the token, a path that is no route answers 404: what is checked here is the token alone.
func TestTheAccessTokenIsTakenBareOrAsBearer(t *testing.T) {
	logger.LogInitConsoleOnly()
	runtime := t.TempDir()
	previous := config.CommonInfo.RuntimePath
	config.CommonInfo.RuntimePath = runtime
	t.Cleanup(func() { config.CommonInfo.RuntimePath = previous })
	if err := os.WriteFile(filepath.Join(runtime, external.InternalSecretFilename), []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	privateKey, publicKey, err := jwt.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	jwks, err := jwt.GenerateJwksJSON(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	userService := httptest.NewServer(jwt.JWKSHandler(jwks))
	defer userService.Close()
	if err := os.WriteFile(filepath.Join(runtime, external.UserServiceAddressFilename), []byte(userService.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := jwt.GetAccessToken("admin", privateKey, 1)
	if err != nil {
		t.Fatal(err)
	}

	router := InitV2Router(nil, &service.VMs{}, nil)
	const nothing, console = "/v2/virt_management/nothing-here", "/v2/virt_management/vms/x/console"
	for _, c := range []struct {
		name, target, authorization string
		passes                      bool
	}{
		{"bare", nothing, token, true},
		{"Bearer", nothing, "Bearer " + token, true},
		{"bearer", nothing, "bearer " + token, true},
		{"Basic", nothing, "Basic " + token, false},
		{"the scheme alone", nothing, "Bearer", false},
		{"console ?token= without a header", console + "?token=" + token, "", true},
		{"console, the scheme alone and ?token=: the header decides", console + "?token=" + token, "Bearer", false},
	} {
		request := httptest.NewRequest(http.MethodGet, c.target, nil)
		request.RemoteAddr = "203.0.113.5:40000"
		if c.authorization != "" {
			request.Header.Set("Authorization", c.authorization)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if passed := recorder.Code != http.StatusUnauthorized; passed != c.passes {
			t.Errorf("%s: %d, passes %v, want %v", c.name, recorder.Code, passed, c.passes)
		}
	}
}
