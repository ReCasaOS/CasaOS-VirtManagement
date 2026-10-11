package route

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestAcceptsTokenInQuery(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{http.MethodGet, V2APIPath + "/vms/:id/console", true},
		{http.MethodPost, V2APIPath + "/vms/:id/console", false},
		{http.MethodGet, V2APIPath + "/host", false},
		{http.MethodGet, V2APIPath + "/vms/:id", false},
		{http.MethodGet, "", false},
	}

	e := echo.New()
	for _, tc := range cases {
		c := e.NewContext(httptest.NewRequest(tc.method, "/", nil), httptest.NewRecorder())
		c.SetPath(tc.path)

		if got := acceptsTokenInQuery(c); got != tc.want {
			t.Errorf("%s %q: got %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}
