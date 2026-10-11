package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"go.uber.org/zap"
)

// Images are downloaded from a URL the user gives, never from one the
// service picks: it keeps no catalogue and contacts nothing by itself.

// partSuffix marks a file still being written; the images list ignores it,
// and a service that starts deletes those a stop left behind.
const partSuffix = ".part"

// keptDownloads is how many downloads the list holds; the oldest finished
// ones go first.
const keptDownloads = 20

// ErrDownloadNotFound is a download id the list does not have.
var ErrDownloadNotFound = fmt.Errorf("%w: no such download", ErrNotFound)

// Downloads runs image downloads into a folder.
type Downloads struct {
	ctx    context.Context
	dir    func() string // the folder, asked each time: the config can say where
	client *http.Client

	mu    sync.Mutex
	items []*download // oldest first
}

type download struct {
	id, url, name, path, sha string
	created                  time.Time
	total                    int64 // -1: unknown
	done                     atomic.Int64
	cancel                   context.CancelFunc

	mu    sync.Mutex
	state codegen.DownloadState
	err   string
}

func NewDownloads(ctx context.Context, dir func() string) *Downloads {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = 60 * time.Second

	return &Downloads{ctx: ctx, dir: dir, client: &http.Client{Transport: transport}}
}

// CleanParts deletes what downloads cut short by a stop left behind.
func (d *Downloads) CleanParts() {
	parts, _ := filepath.Glob(filepath.Join(d.dir(), "*"+partSuffix))
	for _, p := range parts {
		if err := os.Remove(p); err == nil {
			logger.Info("deleted an unfinished download", zap.String("path", p))
		}
	}
}

// Start asks the server for rawURL and, once it answers, goes on in the
// background. The name comes from the answer, so it is checked here.
func (d *Downloads) Start(rawURL, sha string) (codegen.Download, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return codegen.Download{}, fmt.Errorf("%w: give an http:// or https:// address", ErrInvalid)
	}

	sha = strings.ToLower(strings.TrimSpace(sha))
	if sha != "" {
		if b, err := hex.DecodeString(sha); err != nil || len(b) != sha256.Size {
			return codegen.Download{}, fmt.Errorf("%w: a SHA-256 is 64 hex digits", ErrInvalid)
		}
	}

	ctx, cancel := context.WithCancel(d.ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		cancel()
		return codegen.Download{}, fmt.Errorf("%w: %s", ErrInvalid, err)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		cancel()
		return codegen.Download{}, fmt.Errorf("%w: the server could not be reached: %s", ErrInvalid, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return codegen.Download{}, fmt.Errorf("%w: the server answered %s", ErrInvalid, resp.Status)
	}

	name := downloadName(resp)
	if _, _, _, ok := ClassifyImageName(name); !ok || name == "" {
		resp.Body.Close()
		cancel()
		return codegen.Download{}, fmt.Errorf("%w: %q is not an ISO or a disk image (.iso, .qcow2, .img, .raw, .vmdk, .vdi, possibly .xz, .gz or .zip)", ErrInvalid, name)
	}

	dir := d.dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		resp.Body.Close()
		cancel()
		return codegen.Download{}, err
	}

	dst := filepath.Join(dir, name)
	if _, err := os.Stat(dst); err == nil {
		resp.Body.Close()
		cancel()
		return codegen.Download{}, fmt.Errorf("%w: %s is there already", ErrConflict, dst)
	}

	if resp.ContentLength > 0 {
		if free, err := freeBytes(dir); err == nil && free < resp.ContentLength {
			resp.Body.Close()
			cancel()
			return codegen.Download{}, fmt.Errorf("%w: the image is %d MiB, %s has %d MiB free", ErrInvalid, resp.ContentLength>>20, dir, free>>20)
		}
	}

	item := &download{
		id: newDownloadID(), url: u.String(), name: name, path: dst, sha: sha,
		created: time.Now().UTC(), total: resp.ContentLength, cancel: cancel,
		state: codegen.Downloading,
	}

	// one file per name: a second download of it would write the same .part
	d.mu.Lock()
	for _, other := range d.items {
		if other.path == dst && other.getState() == codegen.Downloading {
			d.mu.Unlock()
			resp.Body.Close()
			cancel()
			return codegen.Download{}, fmt.Errorf("%w: %s is being downloaded already", ErrConflict, name)
		}
	}
	d.items = append(d.items, item)
	d.trim()
	d.mu.Unlock()

	logger.Info("downloading an image", zap.String("url", item.url), zap.String("path", dst))

	go item.run(resp)

	return item.view(), nil
}

// run writes the body to <path>.part, checks it, and renames it.
func (item *download) run(resp *http.Response) {
	defer resp.Body.Close()
	defer item.cancel()

	part := item.path + partSuffix
	err := func() error {
		f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()

		var sum hash.Hash
		var w io.Writer = f
		if item.sha != "" {
			sum = sha256.New()
			w = io.MultiWriter(f, sum)
		}

		if _, err := io.Copy(w, &counter{r: resp.Body, n: &item.done}); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if item.total > 0 && item.done.Load() != item.total {
			return fmt.Errorf("the server sent %d of %d bytes", item.done.Load(), item.total)
		}

		if sum != nil {
			item.setState(codegen.Verifying, "")
			if got := hex.EncodeToString(sum.Sum(nil)); got != item.sha {
				return fmt.Errorf("the SHA-256 is %s, not the one given", got)
			}
		}

		// canceled while the checksum was being checked: nothing to keep
		if item.getState() == codegen.Canceled {
			return context.Canceled
		}

		return os.Rename(part, item.path)
	}()

	if err == nil {
		item.setState(codegen.Done, "")
		logger.Info("downloaded an image", zap.String("path", item.path), zap.Int64("bytes", item.done.Load()))
		return
	}

	os.Remove(part)

	if errors.Is(err, context.Canceled) && item.getState() == codegen.Canceled {
		logger.Info("canceled an image download", zap.String("url", item.url))
		return
	}

	item.setState(codegen.Failed, err.Error())
	logger.Error("error when downloading an image", zap.String("url", item.url), zap.Error(err))
}

// List is every download the list holds, oldest first.
func (d *Downloads) List() []codegen.Download {
	d.mu.Lock()
	defer d.mu.Unlock()

	out := make([]codegen.Download, 0, len(d.items))
	for _, item := range d.items {
		out = append(out, item.view())
	}
	return out
}

// Delete cancels a running download, or takes a finished one off the list.
func (d *Downloads) Delete(id string) (codegen.Download, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for i, item := range d.items {
		if item.id != id {
			continue
		}

		if s := item.getState(); s == codegen.Downloading || s == codegen.Verifying {
			item.setState(codegen.Canceled, "")
			item.cancel()
			return item.view(), nil
		}

		d.items = append(d.items[:i], d.items[i+1:]...)
		return item.view(), nil
	}

	return codegen.Download{}, ErrDownloadNotFound
}

// trim drops the oldest finished downloads past keptDownloads; d.mu is held.
func (d *Downloads) trim() {
	for len(d.items) > keptDownloads {
		i := 0
		for i < len(d.items) && (d.items[i].getState() == codegen.Downloading || d.items[i].getState() == codegen.Verifying) {
			i++
		}
		if i == len(d.items) {
			return
		}
		d.items = append(d.items[:i], d.items[i+1:]...)
	}
}

func (item *download) setState(s codegen.DownloadState, errText string) {
	item.mu.Lock()
	defer item.mu.Unlock()

	// a cancel wins over what the copy that it stopped says
	if item.state == codegen.Canceled {
		return
	}
	item.state, item.err = s, errText
}

func (item *download) getState() codegen.DownloadState {
	item.mu.Lock()
	defer item.mu.Unlock()
	return item.state
}

func (item *download) view() codegen.Download {
	item.mu.Lock()
	defer item.mu.Unlock()

	v := codegen.Download{
		Id: item.id, Url: item.url, Name: item.name, Path: item.path,
		State: item.state, DoneBytes: item.done.Load(), CreatedAt: item.created,
	}
	if item.total > 0 {
		total := item.total
		v.TotalBytes = &total
	}
	if item.err != "" {
		e := item.err
		v.Error = &e
	}
	return v
}

// downloadName is the file name the server gives (Content-Disposition), or
// the last part of the address it was downloaded from, redirects followed;
// never a path.
func downloadName(resp *http.Response) string {
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		if n := safeName(params["filename"]); n != "" {
			return n
		}
	}

	if resp.Request != nil && resp.Request.URL != nil {
		return safeName(path.Base(resp.Request.URL.Path))
	}
	return ""
}

func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if name == "." || name == "/" || strings.HasPrefix(name, ".") {
		return ""
	}
	return name
}

func newDownloadID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// counter counts what goes through it into n.
type counter struct {
	r io.Reader
	n *atomic.Int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
