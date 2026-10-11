package service

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
)

// DataRoot is where every image must be: the root of the file browser.
var DataRoot = "/DATA"

// ISOFolderName is the folder in the storage path offered for images.
const ISOFolderName = "iso"

// Image is a validated installation image.
type Image struct {
	Path string // cleaned, symlinks resolved

	// ISO is attached as a CD-ROM; anything else is a disk image to import.
	ISO bool

	// Format is qemu-img's name for the (decompressed) disk image: qcow2,
	// raw, vmdk or vdi. Empty for an ISO.
	Format string

	// Compression is "xz", "gz", "zip" or empty.
	Compression string

	Size int64
}

var diskFormats = map[string]string{
	".qcow2": "qcow2",
	".img":   "raw",
	".raw":   "raw",
	".vmdk":  "vmdk",
	".vdi":   "vdi",
}

var compressions = map[string]string{
	".xz":  "xz",
	".gz":  "gz",
	".zip": "zip",
}

// ClassifyImageName says what an image is from its file name. A .zip is
// taken to hold a disk image; which one is decided when it is opened.
func ClassifyImageName(name string) (iso bool, format, compression string, ok bool) {
	lower := strings.ToLower(name)
	ext := filepath.Ext(lower)

	if ext == ".iso" {
		return true, "", "", true
	}

	if c, found := compressions[ext]; found {
		if c == "zip" {
			return false, "", c, true
		}

		inner := filepath.Ext(strings.TrimSuffix(lower, ext))
		if f, found := diskFormats[inner]; found {
			return false, f, c, true
		}

		return false, "", "", false
	}

	if f, found := diskFormats[ext]; found {
		return false, f, "", true
	}

	return false, "", "", false
}

// ValidateImagePath checks that path is an image the service may read: an
// absolute path which, cleaned and with symlinks resolved, is still under
// DataRoot, names a regular file, and has a known extension.
func ValidateImagePath(path string) (Image, error) {
	if !filepath.IsAbs(path) {
		return Image{}, fmt.Errorf("%w: image path %q is not absolute", ErrInvalid, path)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return Image{}, fmt.Errorf("%w: image %q: %s", ErrInvalid, path, notExistOr(err))
	}

	root, err := filepath.EvalSymlinks(DataRoot)
	if err != nil {
		return Image{}, fmt.Errorf("%w: %s: %s", ErrInvalid, DataRoot, notExistOr(err))
	}

	if !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return Image{}, fmt.Errorf("%w: image %q is not under %s", ErrInvalid, path, DataRoot)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return Image{}, fmt.Errorf("%w: image %q: %s", ErrInvalid, path, notExistOr(err))
	}

	if !info.Mode().IsRegular() {
		return Image{}, fmt.Errorf("%w: image %q is not a regular file", ErrInvalid, path)
	}

	iso, format, compression, ok := ClassifyImageName(resolved)
	if !ok {
		return Image{}, fmt.Errorf("%w: %q is neither an ISO nor a disk image (.qcow2, .img, .raw, .vmdk, .vdi, optionally .xz, .gz or .zip)", ErrInvalid, filepath.Base(path))
	}

	return Image{Path: resolved, ISO: iso, Format: format, Compression: compression, Size: info.Size()}, nil
}

func notExistOr(err error) string {
	if os.IsNotExist(err) {
		return "no such file"
	}

	return err.Error()
}

// ListImages lists the ISOs and disk images in the iso folder of storagePath,
// by name. A missing folder is an empty list.
func ListImages(storagePath string) ([]codegen.Image, error) {
	dir := filepath.Join(storagePath, ISOFolderName)

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []codegen.Image{}, nil
	}
	if err != nil {
		return nil, err
	}

	images := []codegen.Image{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		iso, _, _, ok := ClassifyImageName(entry.Name())
		if !ok {
			continue
		}

		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}

		kind := codegen.ImageKindDisk
		if iso {
			kind = codegen.ImageKindIso
		}

		size := info.Size()
		images = append(images, codegen.Image{
			Path:      filepath.Join(dir, entry.Name()),
			Name:      entry.Name(),
			Kind:      kind,
			SizeBytes: &size,
		})
	}

	sort.Slice(images, func(i, j int) bool { return strings.ToLower(images[i].Name) < strings.ToLower(images[j].Name) })

	return images, nil
}
