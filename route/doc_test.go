package route

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const docYAML = `openapi: 3.0.0
info:
  title: doc
`

func TestDocRouterServesOpenAPIOnly(t *testing.T) {
	h := InitV2DocRouter(docYAML)
	for path, want := range map[string]int{
		V2DocPath:                   http.StatusOK,
		V2DocPath + "/":             http.StatusOK,
		V2DocPath + "/openapi.yaml": http.StatusOK,
		V2DocPath + "/index.html":   http.StatusNotFound,
		V2DocPath + "/other":        http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Fatalf("%s: status %d, want %d", path, rec.Code, want)
		}
		if want != http.StatusOK {
			continue
		}
		if rec.Body.String() != docYAML {
			t.Fatalf("%s: body %q", path, rec.Body.String())
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s: X-Content-Type-Options %q", path, got)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/yaml" {
			t.Fatalf("%s: Content-Type %q", path, got)
		}
	}
}

// No file the service embeds from api/ loads anything from another host.
func TestEmbeddedDocLoadsNothingRemote(t *testing.T) {
	err := filepath.WalkDir("../api", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, bad := range []string{"<script src=\"http", "<link", "cdn.jsdelivr", "unpkg.com"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
