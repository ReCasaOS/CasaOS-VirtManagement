package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
)

func init() {
	logger.LogInitConsoleOnly()
}

func waitState(t *testing.T, d *Downloads, id string, want codegen.DownloadState) codegen.Download {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, v := range d.List() {
			if v.Id == id && v.State == want {
				return v
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("download %s never got to %s: %+v", id, want, d.List())
	return codegen.Download{}
}

func TestDownloads(t *testing.T) {
	body := []byte("an ISO, in spirit")
	sum := sha256.Sum256(body)
	release := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/alpine.iso", func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	mux.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="../../etc/haos_ova-13.qcow2.xz"`)
		w.Write(body)
	})
	mux.HandleFunc("/notes.txt", func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	mux.HandleFunc("/slow.iso", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write(body)
		w.(http.Flusher).Flush()
		<-release
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer close(release)

	dir := t.TempDir()
	d := NewDownloads(context.Background(), func() string { return dir })

	t.Run("downloads and checks the checksum", func(t *testing.T) {
		v, err := d.Start(srv.URL+"/alpine.iso", hex.EncodeToString(sum[:]))
		if err != nil {
			t.Fatal(err)
		}
		if v.Name != "alpine.iso" || v.Path != filepath.Join(dir, "alpine.iso") {
			t.Errorf("name %q path %q", v.Name, v.Path)
		}
		got := waitState(t, d, v.Id, codegen.Done)
		if got.DoneBytes != int64(len(body)) {
			t.Errorf("done %d", got.DoneBytes)
		}
		if b, _ := os.ReadFile(v.Path); string(b) != string(body) {
			t.Errorf("file %q", b)
		}
	})

	t.Run("never overwrites", func(t *testing.T) {
		if _, err := d.Start(srv.URL+"/alpine.iso", ""); !errors.Is(err, ErrConflict) {
			t.Errorf("err %v", err)
		}
	})

	t.Run("takes the server's name, without its path", func(t *testing.T) {
		v, err := d.Start(srv.URL+"/get", "")
		if err != nil {
			t.Fatal(err)
		}
		if v.Name != "haos_ova-13.qcow2.xz" || filepath.Dir(v.Path) != dir {
			t.Errorf("name %q path %q", v.Name, v.Path)
		}
		waitState(t, d, v.Id, codegen.Done)
	})

	t.Run("deletes a file whose checksum differs", func(t *testing.T) {
		os.Remove(filepath.Join(dir, "alpine.iso"))
		v, err := d.Start(srv.URL+"/alpine.iso", "00"+hex.EncodeToString(sum[1:]))
		if err != nil {
			t.Fatal(err)
		}
		got := waitState(t, d, v.Id, codegen.Failed)
		if got.Error == nil {
			t.Error("no error")
		}
		if _, err := os.Stat(v.Path); !os.IsNotExist(err) {
			t.Error("the file stayed")
		}
		if _, err := os.Stat(v.Path + partSuffix); !os.IsNotExist(err) {
			t.Error("the .part stayed")
		}
	})

	t.Run("refuses what is not an image, a 404, and other schemes", func(t *testing.T) {
		for _, u := range []string{srv.URL + "/notes.txt", srv.URL + "/missing.iso", "ftp://example.com/a.iso", "file:///etc/passwd"} {
			if _, err := d.Start(u, ""); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: err %v", u, err)
			}
		}
		if _, err := d.Start(srv.URL+"/alpine.iso", "xyz"); !errors.Is(err, ErrInvalid) {
			t.Errorf("bad sha: %v", err)
		}
	})

	t.Run("cancels and deletes what it wrote, then dismisses", func(t *testing.T) {
		v, err := d.Start(srv.URL+"/slow.iso", "")
		if err != nil {
			t.Fatal(err)
		}
		if v.TotalBytes == nil || *v.TotalBytes != 1000 {
			t.Errorf("total %v", v.TotalBytes)
		}
		if _, err := d.Delete(v.Id); err != nil {
			t.Fatal(err)
		}
		waitState(t, d, v.Id, codegen.Canceled)
		time.Sleep(50 * time.Millisecond)
		if _, err := os.Stat(v.Path + partSuffix); !os.IsNotExist(err) {
			t.Error("the .part stayed")
		}
		if _, err := d.Delete(v.Id); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Delete(v.Id); !errors.Is(err, ErrNotFound) {
			t.Errorf("dismissed twice: %v", err)
		}
	})
}

func TestCleanParts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.iso.part"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "b.iso"), nil, 0o644)

	NewDownloads(context.Background(), func() string { return dir }).CleanParts()

	if _, err := os.Stat(filepath.Join(dir, "a.iso.part")); !os.IsNotExist(err) {
		t.Error("the .part stayed")
	}
	if _, err := os.Stat(filepath.Join(dir, "b.iso")); err != nil {
		t.Error("an image went")
	}
}
