package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"go.uber.org/zap"
	"golang.org/x/text/unicode/norm"
)

// Images uploaded from the browser go where downloads go, through the same
// <name>.part, which CleanParts deletes at start.

const (
	maxUploads      = 2
	uploadIdle      = 2 * time.Minute // between two reads
	uploadMinRate   = 128 << 10       // bytes per second, over the whole upload
	uploadSlack     = 5 * time.Minute
	uploadMaxTime   = 24 * time.Hour
	uploadReserve   = 1 << 30 // and at least 2 % of the filesystem
	maxUploadName   = 255 - len(partSuffix)
	isoSignatureAt  = 32769 // sector 16, after its type byte
	warningNoISOSig = "iso_signature_missing"
)

// UploadError is a refused upload: the status and the code the route answers.
type UploadError struct {
	Status int
	Code   string
	Err    error // the cause, for the log; never sent
}

func (e *UploadError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Err.Error()
	}
	return e.Code
}

func (e *UploadError) Unwrap() error { return e.Err }

func refuse(status int, code string, err error) *UploadError {
	return &UploadError{Status: status, Code: code, Err: err}
}

var (
	errClaimed = errors.New("the name is being written")
	errNoRoom  = errors.New("not enough free space")
)

// claims are the files being written into the iso folder, by downloads and
// uploads, and the bytes each will take (0 when unknown).
type claims struct {
	mu    sync.Mutex
	names map[string]int64
}

func newClaims() *claims { return &claims{names: map[string]int64{}} }

// claim takes path for a writer of n bytes: errClaimed if another writer has
// it; with avail >= 0, errNoRoom if n and the bytes claimed exceed avail.
func (c *claims) claim(path string, n, avail int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, taken := c.names[path]; taken {
		return errClaimed
	}
	if avail >= 0 {
		claimed := int64(0)
		for _, b := range c.names {
			claimed += b
		}
		if n > avail-claimed {
			return errNoRoom
		}
	}
	c.names[path] = n
	return nil
}

func (c *claims) release(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.names, path)
}

func (c *claims) empty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.names) == 0
}

func responseReadDeadline(w http.ResponseWriter, t time.Time) error {
	return http.NewResponseController(w).SetReadDeadline(t)
}

// Uploads writes request bodies into the folder of a Downloads, at most
// maxUploads at once.
type Uploads struct {
	dir      func() string
	claims   *claims // shared with the downloads
	maxBytes int64
	idle     time.Duration
	now      func() time.Time
	space    func(path string) (free, total int64, err error)

	// setReadDeadline bounds the wait for the next bytes of the body.
	setReadDeadline func(w http.ResponseWriter, t time.Time) error

	beforeRename func() // tests only

	slots chan struct{}
}

func NewUploads(d *Downloads, maxBytes int64) *Uploads {
	return &Uploads{
		dir: d.dir, claims: d.claims, maxBytes: maxBytes, idle: uploadIdle, now: time.Now, space: diskSpace,
		setReadDeadline: responseReadDeadline, beforeRename: func() {},
		slots: make(chan struct{}, maxUploads),
	}
}

// uploadDeadline is when an upload of length bytes started at start must be
// over: at uploadMinRate, plus some slack, and never more than a day.
func uploadDeadline(start time.Time, length int64) time.Time {
	return start.Add(min(time.Duration(length/uploadMinRate)*time.Second+uploadSlack, uploadMaxTime))
}

// windowsDevices are names Windows cannot open, whatever the extension: /DATA
// is shared with Windows machines over SMB.
var windowsDevices = map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}

func init() {
	for i := 1; i <= 9; i++ {
		windowsDevices[fmt.Sprintf("com%d", i)] = true
		windowsDevices[fmt.Sprintf("lpt%d", i)] = true
	}
}

// UploadName is raw as a file name for the iso folder, in Unicode NFC, or
// false: a plain name with an image extension, nothing that hides it, splits
// it into a path or makes it read differently from what it is.
func UploadName(raw string) (string, bool) {
	if !utf8.ValidString(raw) {
		return "", false
	}
	name := norm.NFC.String(raw)
	if name == "" || len(name) > maxUploadName || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") ||
		strings.TrimSpace(name) != name {
		return "", false
	}
	for _, r := range name {
		// control and format characters (NUL, newlines, bidi overrides,
		// zero-width spaces), and both separators
		if r == '/' || r == '\\' || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", false
		}
	}
	if stem, _, _ := strings.Cut(name, "."); windowsDevices[strings.ToLower(stem)] {
		return "", false
	}
	if _, _, _, ok := ClassifyImageName(name); !ok {
		return "", false
	}
	return name, true
}

// Receive writes the body of r to the iso folder as name, checks it, and
// gives the image. Everything that can be refused without the body is refused
// before it is read. Errors are *UploadError but for unexpected ones.
func (u *Uploads) Receive(w http.ResponseWriter, r *http.Request, rawName, sha string) (codegen.Image, error) {
	name, ok := UploadName(rawName)
	if !ok {
		return codegen.Image{}, refuse(http.StatusBadRequest, "bad_name", nil)
	}

	sha = strings.ToLower(sha)
	if sha != "" {
		if b, err := hex.DecodeString(sha); err != nil || len(b) != sha256.Size {
			return codegen.Image{}, refuse(http.StatusBadRequest, "bad_sha256", nil)
		}
	}

	length := r.ContentLength
	if length <= 0 {
		return codegen.Image{}, refuse(http.StatusLengthRequired, "length_required", nil)
	}
	if length > u.maxBytes {
		return codegen.Image{}, refuse(http.StatusRequestEntityTooLarge, "too_large", nil)
	}

	select {
	case u.slots <- struct{}{}:
		defer func() { <-u.slots }()
	default:
		return codegen.Image{}, refuse(http.StatusServiceUnavailable, "busy", nil)
	}

	dir := u.dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return codegen.Image{}, err
	}

	dst := filepath.Join(dir, name)
	if _, err := os.Lstat(dst); err == nil {
		return codegen.Image{}, refuse(http.StatusConflict, "exists", nil)
	}

	free, total, err := u.space(dir)
	if err != nil {
		return codegen.Image{}, err
	}
	// the bytes the running uploads and downloads will still take are not free
	switch err := u.claims.claim(dst, length, max(free-max(uploadReserve, total/50), 0)); {
	case errors.Is(err, errClaimed):
		return codegen.Image{}, refuse(http.StatusConflict, "exists", nil)
	case errors.Is(err, errNoRoom):
		return codegen.Image{}, refuse(http.StatusInsufficientStorage, "insufficient_storage", nil)
	}
	defer u.claims.release(dst)

	// O_EXCL: a .part left by anything else is not ours to write or delete
	part := dst + partSuffix
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return codegen.Image{}, refuse(http.StatusConflict, "exists", nil)
	}
	if err != nil {
		return codegen.Image{}, err
	}
	ours := true // the part path is still the file written: delete it on failure, panics included
	defer func() {
		f.Close()
		if ours {
			os.Remove(part)
		}
	}()

	logger.Info("receiving an image", zap.String("name", fmt.Sprintf("%q", name)), zap.Int64("bytes", length))

	img, err := u.write(w, r.Body, f, &ours, length, sha, dst)
	if err != nil {
		logger.Info("an image upload failed", zap.String("name", fmt.Sprintf("%q", name)), zap.Error(err))
		return codegen.Image{}, err
	}

	logger.Info("received an image", zap.String("name", fmt.Sprintf("%q", name)), zap.Int64("bytes", length))
	return img, nil
}

// write copies exactly length bytes of body into f (dst's .part), checks
// that file and renames it to dst. *ours goes false once the part path no
// longer names f.
func (u *Uploads) write(w http.ResponseWriter, body io.Reader, f *os.File, ours *bool, length int64, sha, dst string) (codegen.Image, error) {
	part := f.Name()
	sum := sha256.New()
	end := uploadDeadline(u.now(), length)
	in := &idleReader{r: io.LimitReader(body, length+1), extend: func() error {
		next := u.now().Add(u.idle)
		if next.After(end) {
			next = end
		}
		return u.setReadDeadline(w, next)
	}}

	n, err := io.CopyBuffer(io.MultiWriter(f, sum), in, make([]byte, 1<<20))
	switch {
	case in.deadlineErr != nil:
		return codegen.Image{}, in.deadlineErr
	case errors.Is(err, os.ErrDeadlineExceeded):
		return codegen.Image{}, refuse(http.StatusRequestTimeout, "stalled", err)
	case errors.Is(err, syscall.ENOSPC):
		return codegen.Image{}, refuse(http.StatusInsufficientStorage, "insufficient_storage", err)
	case in.readErr != nil:
		return codegen.Image{}, refuse(http.StatusBadRequest, "incomplete", err) // the client went
	case err != nil:
		return codegen.Image{}, err
	case n < length:
		return codegen.Image{}, refuse(http.StatusBadRequest, "incomplete", nil)
	case n > length:
		return codegen.Image{}, refuse(http.StatusBadRequest, "too_long", nil)
	}
	if err := f.Sync(); errors.Is(err, syscall.ENOSPC) {
		return codegen.Image{}, refuse(http.StatusInsufficientStorage, "insufficient_storage", err)
	} else if err != nil {
		return codegen.Image{}, err
	}

	got := hex.EncodeToString(sum.Sum(nil))
	if sha != "" && got != sha {
		return codegen.Image{}, refuse(http.StatusUnprocessableEntity, "checksum_mismatch", nil)
	}

	// the file written, never whatever the path names now
	warning, err := checkMagic(f, dst)
	if err != nil {
		return codegen.Image{}, err
	}

	u.beforeRename()

	written, err := f.Stat()
	if err != nil {
		return codegen.Image{}, err
	}
	if now, err := os.Lstat(part); err != nil || !os.SameFile(written, now) {
		*ours = false // what is there is someone else's; ours goes with f
		return codegen.Image{}, refuse(http.StatusInternalServerError, "conflict", errors.New("the .part was replaced while it was written"))
	}

	if err := renameNoReplace(part, dst); errors.Is(err, os.ErrExist) {
		return codegen.Image{}, refuse(http.StatusConflict, "exists", nil)
	} else if err != nil {
		return codegen.Image{}, err
	}
	*ours = false
	syncDir(filepath.Dir(dst))

	image, err := ValidateImagePath(dst)
	if err != nil {
		os.Remove(dst)
		return codegen.Image{}, err
	}

	kind := codegen.ImageKindDisk
	if image.ISO {
		kind = codegen.ImageKindIso
	}
	out := codegen.Image{Path: dst, Name: filepath.Base(dst), Kind: kind, SizeBytes: &image.Size, Sha256: &got}
	if warning != "" {
		out.Warning = &warning
	}
	return out, nil
}

// renameChecked renames from to to unless to exists (os.ErrExist). Not
// atomic: for filesystems that cannot refuse to replace by themselves.
func renameChecked(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return os.ErrExist
	}
	return os.Rename(from, to)
}

// syncDir makes a rename in dir durable, as far as the filesystem allows.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// checkMagic looks at the start of an image named name. A .qcow2 without the
// qcow2 magic is refused. An .iso without an ISO 9660 (CD001) or UDF (BEA01,
// NSR02, NSR03) volume descriptor at byte 32769 gets a warning and is kept:
// what boots is the firmware's business, and odd images do exist. Compressed
// and other images are taken by their extension.
func checkMagic(f io.ReaderAt, name string) (warning string, err error) {
	iso, format, compression, _ := ClassifyImageName(name)
	switch {
	case iso:
		sig := make([]byte, 5)
		if _, err := f.ReadAt(sig, isoSignatureAt); err == nil {
			for _, want := range []string{"CD001", "BEA01", "NSR02", "NSR03"} {
				if string(sig) == want {
					return "", nil
				}
			}
		}
		return warningNoISOSig, nil
	case format == "qcow2" && compression == "":
		magic := make([]byte, 4)
		if _, err := f.ReadAt(magic, 0); err != nil || !bytes.Equal(magic, []byte("QFI\xfb")) {
			return "", refuse(http.StatusUnprocessableEntity, "bad_image", nil)
		}
	}
	return "", nil
}

// idleReader pushes the read deadline back before each read, and tells a
// failed read from a failed write.
type idleReader struct {
	r                    io.Reader
	extend               func() error
	readErr, deadlineErr error
}

func (i *idleReader) Read(p []byte) (int, error) {
	if err := i.extend(); err != nil {
		i.deadlineErr = fmt.Errorf("cannot bound the upload's idle time: %w", err)
		return 0, i.deadlineErr
	}
	n, err := i.r.Read(p)
	if err != nil && err != io.EOF {
		i.readErr = err
	}
	return n, err
}
