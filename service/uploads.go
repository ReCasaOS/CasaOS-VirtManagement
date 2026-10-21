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
	uploadIdle      = 2 * time.Minute
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

// Uploads writes request bodies into a folder, at most maxUploads at once.
type Uploads struct {
	dir      func() string // asked each time: the config can say where
	maxBytes int64
	idle     time.Duration
	space    func(path string) (free, total int64, err error)

	// setReadDeadline bounds the wait for the next bytes of the body.
	setReadDeadline func(w http.ResponseWriter, t time.Time) error

	slots chan struct{}
}

func NewUploads(dir func() string, maxBytes int64) *Uploads {
	return &Uploads{
		dir: dir, maxBytes: maxBytes, idle: uploadIdle, space: diskSpace,
		setReadDeadline: func(w http.ResponseWriter, t time.Time) error {
			return http.NewResponseController(w).SetReadDeadline(t)
		},
		slots: make(chan struct{}, maxUploads),
	}
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
	if length > free-max(uploadReserve, total/50) {
		return codegen.Image{}, refuse(http.StatusInsufficientStorage, "insufficient_storage", nil)
	}

	// O_EXCL: an upload or a download of this name holds its .part
	part := dst + partSuffix
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if errors.Is(err, os.ErrExist) {
		return codegen.Image{}, refuse(http.StatusConflict, "exists", nil)
	}
	if err != nil {
		return codegen.Image{}, err
	}

	logger.Info("receiving an image", zap.String("name", fmt.Sprintf("%q", name)), zap.Int64("bytes", length))

	img, err := u.write(w, r.Body, f, length, sha, dst)
	f.Close()
	if err != nil {
		os.Remove(part)
		logger.Info("an image upload failed", zap.String("name", fmt.Sprintf("%q", name)), zap.Error(err))
		return codegen.Image{}, err
	}

	logger.Info("received an image", zap.String("name", fmt.Sprintf("%q", name)), zap.Int64("bytes", length))
	return img, nil
}

// write copies exactly length bytes of body into f (dst's .part), checks
// them and renames f to dst.
func (u *Uploads) write(w http.ResponseWriter, body io.Reader, f *os.File, length int64, sha, dst string) (codegen.Image, error) {
	sum := sha256.New()
	in := &idleReader{r: io.LimitReader(body, length+1), extend: func() error {
		return u.setReadDeadline(w, time.Now().Add(u.idle))
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
	if err := f.Close(); err != nil {
		return codegen.Image{}, err
	}

	got := hex.EncodeToString(sum.Sum(nil))
	if sha != "" && got != sha {
		return codegen.Image{}, refuse(http.StatusUnprocessableEntity, "checksum_mismatch", nil)
	}

	r, err := os.Open(f.Name())
	if err != nil {
		return codegen.Image{}, err
	}
	warning, err := checkMagic(r, dst)
	r.Close()
	if err != nil {
		return codegen.Image{}, err
	}

	// ponytail: Lstat then Rename is not atomic against a file someone puts
	// there by hand meanwhile; every writer of the service holds the .part.
	// renameat2(RENAME_NOREPLACE) if that ever matters.
	if _, err := os.Lstat(dst); err == nil {
		return codegen.Image{}, refuse(http.StatusConflict, "exists", nil)
	}
	if err := os.Rename(f.Name(), dst); err != nil {
		return codegen.Image{}, err
	}

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
