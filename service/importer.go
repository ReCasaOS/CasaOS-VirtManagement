package service

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ulikunitz/xz"
)

// importDisk copies the disk image img into a new qcow2 at dst, at least
// minBytes large. The source is only read. Compressed images are unpacked
// next to dst first, then qemu-img converts; everything is written to
// temporary names in dst's folder and dst appears only once it is complete.
func importDisk(ctx context.Context, img Image, dst string, minBytes int64) (err error) {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		return errors.New("qemu-img is not installed (package qemu-utils)")
	}

	dir, base := filepath.Dir(dst), filepath.Base(dst)
	unpacked := filepath.Join(dir, "."+base+".source")
	converted := filepath.Join(dir, "."+base+".convert")

	defer func() {
		os.Remove(unpacked)
		if err != nil {
			os.Remove(converted)
		}
	}()

	src, format := img.Path, img.Format
	if img.Compression != "" {
		if format, err = unpack(ctx, img, unpacked); err != nil {
			return fmt.Errorf("unpack %s: %w", filepath.Base(img.Path), err)
		}
		src = unpacked
	}

	if err := checkSelfContained(ctx, qemuImg, src, format); err != nil {
		return err
	}

	if out, err := exec.CommandContext(ctx, qemuImg, "convert", "-f", format, "-O", "qcow2", src, converted).CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %s", commandError(err, out))
	}

	size, err := virtualSize(ctx, qemuImg, converted, "qcow2")
	if err != nil {
		return err
	}

	if size < minBytes {
		if out, err := exec.CommandContext(ctx, qemuImg, "resize", "-f", "qcow2", converted, strconv.FormatInt(minBytes, 10)).CombinedOutput(); err != nil {
			return fmt.Errorf("qemu-img resize: %s", commandError(err, out))
		}
	}

	if err := os.Chmod(converted, 0o600); err != nil {
		return err
	}

	return os.Rename(converted, dst)
}

// unpack writes the disk image inside a compressed img to dst and returns its
// format.
func unpack(ctx context.Context, img Image, dst string) (string, error) {
	f, err := os.Open(img.Path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var (
		r      io.Reader
		format = img.Format
	)

	switch img.Compression {
	case "gz":
		gz, err := gzip.NewReader(f)
		if err != nil {
			return "", err
		}
		defer gz.Close()
		r = gz

	case "xz":
		x, err := xz.NewReader(f)
		if err != nil {
			return "", err
		}
		r = x

	case "zip":
		info, err := f.Stat()
		if err != nil {
			return "", err
		}

		zr, err := zip.NewReader(f, info.Size())
		if err != nil {
			return "", err
		}

		entry, entryFormat, err := diskInZip(zr)
		if err != nil {
			return "", err
		}

		rc, err := entry.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		r, format = rc, entryFormat

	default:
		return "", fmt.Errorf("unknown compression %q", img.Compression)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}

	if _, err := io.Copy(out, &ctxReader{ctx: ctx, r: r}); err != nil {
		out.Close()
		return "", err
	}

	return format, out.Close()
}

// diskInZip is the one disk image in a zip archive.
func diskInZip(zr *zip.Reader) (*zip.File, string, error) {
	var (
		found  *zip.File
		format string
	)

	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() {
			continue
		}

		iso, f, compression, ok := ClassifyImageName(entry.Name)
		if !ok || iso || compression != "" {
			continue
		}

		if found != nil {
			return nil, "", fmt.Errorf("the archive holds more than one disk image (%s, %s)", found.Name, entry.Name)
		}
		found, format = entry, f
	}

	if found == nil {
		return nil, "", errors.New("the archive holds no disk image (.qcow2, .img, .raw, .vmdk, .vdi)")
	}

	return found, format, nil
}

type imageInfo struct {
	VirtualSize     int64  `json:"virtual-size"`
	BackingFilename string `json:"backing-filename"`
	FormatSpecific  struct {
		Data struct {
			DataFile string `json:"data-file"`
			Extents  []struct {
				Filename string `json:"filename"`
			} `json:"extents"`
		} `json:"data"`
	} `json:"format-specific"`
}

func readImageInfo(ctx context.Context, qemuImg, path, format string) (imageInfo, error) {
	var info imageInfo

	out, err := exec.CommandContext(ctx, qemuImg, "info", "--output=json", "-f", format, path).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return info, fmt.Errorf("qemu-img info: %s", commandError(err, exitErr.Stderr))
		}
		return info, fmt.Errorf("qemu-img info: %w", err)
	}

	if err := json.Unmarshal(out, &info); err != nil {
		return info, fmt.Errorf("qemu-img info: %w", err)
	}

	return info, nil
}

// checkSelfContained refuses an image that would make qemu-img read other
// files: a qcow2 with a backing file or an external data file, a vmdk whose
// extents live elsewhere. The service runs as root, and the copy would carry
// whatever those files hold into the new disk.
func checkSelfContained(ctx context.Context, qemuImg, path, format string) error {
	info, err := readImageInfo(ctx, qemuImg, path, format)
	if err != nil {
		return err
	}

	if info.BackingFilename != "" {
		return fmt.Errorf("%w: the image depends on a backing file (%s); convert it to a standalone image first", ErrInvalid, info.BackingFilename)
	}

	if info.FormatSpecific.Data.DataFile != "" {
		return fmt.Errorf("%w: the image keeps its data in another file (%s)", ErrInvalid, info.FormatSpecific.Data.DataFile)
	}

	for _, extent := range info.FormatSpecific.Data.Extents {
		if extent.Filename != "" && extent.Filename != path {
			return fmt.Errorf("%w: the image keeps its data in another file (%s)", ErrInvalid, extent.Filename)
		}
	}

	return nil
}

func virtualSize(ctx context.Context, qemuImg, path, format string) (int64, error) {
	info, err := readImageInfo(ctx, qemuImg, path, format)
	return info.VirtualSize, err
}

func commandError(err error, output []byte) string {
	if msg := strings.TrimSpace(string(output)); msg != "" {
		return msg
	}

	return err.Error()
}

// ctxReader stops a long copy when ctx is done.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}

	return c.r.Read(p)
}
