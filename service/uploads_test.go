package service

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUploadName(t *testing.T) {
	for _, tc := range []struct {
		raw, want string // want "" means refused
	}{
		{"alpine-virt-3.24.2-x86_64.iso", "alpine-virt-3.24.2-x86_64.iso"},
		{"haos_ova-13.qcow2.xz", "haos_ova-13.qcow2.xz"},
		{"Disk Image.VMDK", "Disk Image.VMDK"},
		{"cafe\u0301.iso", "caf\u00e9.iso"}, // NFC
		{"\uff58.iso", "\uff58.iso"},        // fullwidth letters are letters
		{"", ""},
		{".iso", ""},
		{".hidden.iso", ""},
		{"../x.iso", ""},
		{"a/b.iso", ""},
		{"a\\b.iso", ""},
		{"..\\x.iso", ""},
		{"/etc/x.iso", ""},
		{"x\x00.iso", ""},
		{"x\n.iso", ""},
		{"x\x7f.iso", ""},
		{"x\u202egpj.iso", ""}, // right-to-left override
		{"x\u200b.iso", ""},    // zero-width space
		{"x.iso.exe", ""},
		{"x.txt", ""},
		{"x\u2024iso", ""}, // one dot leader, not a dot
		{"x.\u0456so", ""}, // Cyrillic i
		{"x.iso ", ""},
		{" x.iso", ""},
		{"x.iso.", ""},
		{"CON.iso", ""},
		{"con.iso", ""},
		{"lpt1.qcow2", ""},
		{"nul.tar.gz", ""},
		{"console.iso", "console.iso"},
		{"bad\xffutf8.iso", ""},
		{strings.Repeat("a", 10000) + ".iso", ""},
		{strings.Repeat("a", 246) + ".iso", strings.Repeat("a", 246) + ".iso"}, // 250 bytes
		{strings.Repeat("a", 247) + ".iso", ""},
		{strings.Repeat("\u00e9", 123) + ".iso", strings.Repeat("\u00e9", 123) + ".iso"}, // 250 bytes
		{strings.Repeat("\u00e9", 124) + ".iso", ""},                                     // 252 bytes
	} {
		got, ok := UploadName(tc.raw)
		if tc.want == "" {
			if ok {
				t.Errorf("%q: accepted as %q", tc.raw, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("%q: got %q %v, want %q", tc.raw, got, ok, tc.want)
		}
	}
}

func TestImageWarning(t *testing.T) {
	iso := func(sig string) []byte {
		b := make([]byte, 36<<10)
		copy(b[32769:], sig)
		return b
	}
	qcow := append([]byte("QFI\xfb"), make([]byte, 100)...)

	for _, tc := range []struct {
		name    string
		body    []byte
		warning string
		bad     bool
	}{
		{"a.iso", iso("CD001"), "", false},
		{"a.iso", iso("BEA01"), "", false}, // UDF
		{"a.iso", iso("NSR02"), "", false},
		{"a.iso", iso("NSR03"), "", false},
		{"a.iso", iso("XXXXX"), "iso_signature_missing", false},
		{"a.iso", []byte("short"), "iso_signature_missing", false},
		{"a.qcow2", qcow, "", false},
		{"a.qcow2", []byte("QFI\xfa and more"), "", true},
		{"a.qcow2", []byte("Q"), "", true},
		{"a.qcow2.xz", []byte("anything"), "", false}, // compressed: by extension
		{"a.img", []byte("anything"), "", false},
		{"a.vmdk", []byte("anything"), "", false},
	} {
		path := filepath.Join(t.TempDir(), tc.name)
		os.WriteFile(path, tc.body, 0o640)
		f, _ := os.Open(path)
		warning, err := checkMagic(f, tc.name)
		f.Close()
		if warning != tc.warning || (err != nil) != tc.bad {
			t.Errorf("%s %q...: warning %q err %v", tc.name, tc.body[:min(len(tc.body), 8)], warning, err)
		}
	}
}

// rig is an Uploads on a temp dir, with plenty of room and no deadlines.
type rig struct {
	t   *testing.T
	dir string
	u   *Uploads
}

func newRig(t *testing.T) *rig {
	t.Helper()
	root := t.TempDir()
	previous := DataRoot
	DataRoot = root
	t.Cleanup(func() { DataRoot = previous })

	dir := filepath.Join(root, "VM", ISOFolderName)
	u := NewUploads(func() string { return dir }, 64<<30)
	u.space = func(string) (int64, int64, error) { return 100 << 30, 200 << 30, nil }
	return &rig{t: t, dir: dir, u: u}
}

// put calls the handler directly, without a server: the read deadline is a
// no-op.
func (r *rig) put(name, sha string, body io.Reader, length int64) (int, string) {
	r.t.Helper()
	r.u.setReadDeadline = func(http.ResponseWriter, time.Time) error { return nil }
	req := httptest.NewRequest(http.MethodPut, "/images/upload", body)
	req.ContentLength = length
	_, err := r.u.Receive(httptest.NewRecorder(), req, name, sha)
	return statusOf(err)
}

func statusOf(err error) (int, string) {
	if err == nil {
		return http.StatusCreated, ""
	}
	var ue *UploadError
	if errors.As(err, &ue) {
		return ue.Status, ue.Code
	}
	return http.StatusInternalServerError, err.Error()
}

func (r *rig) parts() []string {
	parts, _ := filepath.Glob(filepath.Join(r.dir, "*"+partSuffix))
	return parts
}

func (r *rig) noParts() {
	r.t.Helper()
	if p := r.parts(); len(p) != 0 {
		r.t.Errorf("parts left: %v", p)
	}
}

func TestUploadRefusals(t *testing.T) {
	r := newRig(t)
	body := func(n int) io.Reader { return bytes.NewReader(make([]byte, n)) }

	for _, tc := range []struct {
		what   string
		name   string
		sha    string
		body   io.Reader
		length int64
		status int
		code   string
	}{
		{"hostile name", "../x.iso", "", body(10), 10, 400, "bad_name"},
		{"not an image", "x.iso.exe", "", body(10), 10, 400, "bad_name"},
		{"bad sha", "x.iso", "abc", body(10), 10, 400, "bad_sha256"},
		{"no length", "x.iso", "", body(10), -1, 411, "length_required"},
		{"zero length", "x.iso", "", body(0), 0, 411, "length_required"},
		{"over the cap", "x.iso", "", body(10), 64<<30 + 1, 413, "too_large"},
		{"short body", "x.iso", "", body(10), 20, 400, "incomplete"},
		{"overlong body", "x.iso", "", body(30), 20, 400, "too_long"},
		{"checksum", "x.iso", strings.Repeat("0", 64), body(10), 10, 422, "checksum_mismatch"},
		{"not qcow2", "x.qcow2", "", body(10), 10, 422, "bad_image"},
	} {
		status, code := r.put(tc.name, tc.sha, tc.body, tc.length)
		if status != tc.status || code != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.what, status, code, tc.status, tc.code)
		}
		r.noParts()
		if _, err := os.Stat(filepath.Join(r.dir, "x.iso")); err == nil {
			t.Errorf("%s: the file is there", tc.what)
		}
	}
}

func TestUploadFreeSpace(t *testing.T) {
	r := newRig(t)
	var asked string
	for _, tc := range []struct {
		avail, total, length int64
		status               int
	}{
		{10 << 30, 20 << 30, 9<<30 - 1, 201},    // 1 GiB reserve
		{10 << 30, 20 << 30, 9<<30 + 1, 507},    // into the 1 GiB reserve
		{50 << 30, 1000 << 30, 30<<30 + 1, 507}, // into the 2 % reserve (20 GiB)
		{50 << 30, 1000 << 30, 30 << 30, 201},
	} {
		r.u.space = func(dir string) (int64, int64, error) { asked = dir; return tc.avail, tc.total, nil }
		// a body the handler never reads when it refuses; when it accepts, a
		// short one so the test stays small: incomplete means accepted
		status, code := r.put("x.iso", "", bytes.NewReader([]byte("x")), tc.length)
		if tc.status == 507 && (status != 507 || code != "insufficient_storage") {
			t.Errorf("%+v: %d %s", tc, status, code)
		}
		if tc.status == 201 && code != "incomplete" {
			t.Errorf("%+v: %d %s, want it accepted", tc, status, code)
		}
	}
	if asked != r.dir {
		t.Errorf("statfs on %q, want the iso folder %q", asked, r.dir)
	}

	r.u.space = func(string) (int64, int64, error) { return 0, 0, errors.New("statfs failed") }
	if status, _ := r.put("x.iso", "", bytes.NewReader([]byte("x")), 1); status != 500 {
		t.Errorf("statfs failure: %d", status)
	}
}

func TestUploadNeverOverwrites(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(r.dir, 0o755)
	os.WriteFile(filepath.Join(r.dir, "x.iso"), []byte("keep me"), 0o644)
	os.WriteFile(filepath.Join(r.dir, "y.iso"+partSuffix), []byte("a running download"), 0o644)

	untouched := &readCounter{r: bytes.NewReader([]byte("new"))}
	if status, code := r.put("x.iso", "", untouched, 3); status != 409 || code != "exists" || untouched.n != 0 {
		t.Errorf("existing file: %d %s, %d bytes read", status, code, untouched.n)
	}
	if b, _ := os.ReadFile(filepath.Join(r.dir, "x.iso")); string(b) != "keep me" {
		t.Errorf("overwritten: %q", b)
	}
	if status, code := r.put("y.iso", "", bytes.NewReader([]byte("new")), 3); status != 409 || code != "exists" {
		t.Errorf("part in progress: %d %s", status, code)
	}
	if b, _ := os.ReadFile(filepath.Join(r.dir, "y.iso"+partSuffix)); string(b) != "a running download" {
		t.Errorf("someone else's part touched: %q", b)
	}
}

type readCounter struct {
	r io.Reader
	n int
}

func (c *readCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestUploadSucceeds(t *testing.T) {
	r := newRig(t)
	body := make([]byte, 2<<20)
	copy(body[32769:], "CD001")
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])

	r.u.setReadDeadline = func(http.ResponseWriter, time.Time) error { return nil }
	req := httptest.NewRequest(http.MethodPut, "/images/upload", bytes.NewReader(body))
	img, err := r.u.Receive(httptest.NewRecorder(), req, "debian.iso", strings.ToUpper(sha))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.dir, "debian.iso")
	if img.Path != path || img.Name != "debian.iso" || img.Kind != "iso" || *img.SizeBytes != int64(len(body)) || *img.Sha256 != sha || img.Warning != nil {
		t.Errorf("image %+v", img)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("file %v %v", info, err)
	}
	r.noParts()

	listed, _ := ListImages(filepath.Dir(r.dir))
	if len(listed) != 1 || listed[0].Name != "debian.iso" {
		t.Errorf("listed %+v", listed)
	}

	// no signature: kept, with a warning
	req = httptest.NewRequest(http.MethodPut, "/images/upload", bytes.NewReader([]byte("plain")))
	img, err = r.u.Receive(httptest.NewRecorder(), req, "odd.iso", "")
	if err != nil || img.Warning == nil || *img.Warning != "iso_signature_missing" {
		t.Errorf("odd.iso: %+v %v", img, err)
	}
}

// server serves the upload handler over real TCP, so read deadlines and
// client aborts are the real ones.
func (r *rig) server() string {
	r.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, err := r.u.Receive(w, req, req.URL.Query().Get("name"), "")
		status, code := statusOf(err)
		w.WriteHeader(status)
		fmt.Fprint(w, code)
	}))
	r.t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// begin sends the headers of an upload of length bytes and the first part of
// the body, then leaves the connection open.
func begin(t *testing.T, addr, name string, length int, first string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "PUT /images/upload?name=%s HTTP/1.1\r\nHost: x\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n%s", name, length, first)
	return conn
}

func answer(t *testing.T, conn net.Conn) (int, string) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (r *rig) waitParts(n int) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(r.parts()) != n {
		if time.Now().After(deadline) {
			r.t.Fatalf("parts %v, want %d", r.parts(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUploadStalledClient(t *testing.T) {
	r := newRig(t)
	r.u.idle = 200 * time.Millisecond
	conn := begin(t, r.server(), "slow.iso", 1000, "only ten b")

	started := time.Now()
	status, code := answer(t, conn)
	if status != http.StatusRequestTimeout || code != "stalled" {
		t.Errorf("%d %s", status, code)
	}
	if waited := time.Since(started); waited > 3*time.Second {
		t.Errorf("held for %v", waited)
	}
	r.noParts()
}

func TestUploadClientAborts(t *testing.T) {
	r := newRig(t)
	conn := begin(t, r.server(), "gone.iso", 1000, "only ten b")
	r.waitParts(1)
	conn.Close()
	r.waitParts(0)
	if _, err := os.Stat(filepath.Join(r.dir, "gone.iso")); err == nil {
		t.Error("an aborted upload was kept")
	}
}

func TestUploadConcurrencyLimit(t *testing.T) {
	r := newRig(t)
	addr := r.server()
	a := begin(t, addr, "a.iso", 1000, "x")
	b := begin(t, addr, "b.iso", 1000, "x")
	r.waitParts(2)

	c := begin(t, addr, "c.iso", 1, "x")
	if status, code := answer(t, c); status != http.StatusServiceUnavailable || code != "busy" {
		t.Errorf("third: %d %s", status, code)
	}

	a.Close()
	r.waitParts(1)
	d := begin(t, addr, "d.img", 1, "x")
	if status, code := answer(t, d); status != http.StatusCreated {
		t.Errorf("after one ended: %d %s", status, code)
	}
	b.Close()
	r.waitParts(0)
}

func TestCleanPartsTakesUploads(t *testing.T) {
	r := newRig(t)
	conn := begin(t, r.server(), "left.iso", 1000, "x")
	r.waitParts(1)

	// what a stop leaves behind is what a start deletes
	parts := r.parts()
	NewDownloads(t.Context(), func() string { return r.dir }).CleanParts()
	for _, p := range parts {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s stayed", p)
		}
	}
	conn.Close()
}
