package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// maxShares is how many folders one VM can be given; the API says the same.
const maxShares = 16

// virtiofsd finds the host's virtiofsd; a variable so tests can pretend.
var virtiofsd = libvirt.Virtiofsd

// ValidateShareFolder checks that path is an existing folder strictly under
// StorageRoots, symlinks resolved, and returns it cleaned. The roots
// themselves are refused: all of /DATA would hand the guest every app's data.
func ValidateShareFolder(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: give an absolute path", ErrInvalid)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %s does not exist", ErrInvalid, path)
		}
		return "", err
	}

	st, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%w: %s is not a folder", ErrInvalid, path)
	}

	if !underStorageRoots(resolved) || slices.Contains(StorageRoots, resolved) {
		return "", fmt.Errorf("%w: a shared folder goes inside %s", ErrInvalid, strings.Join(StorageRoots, " or "))
	}

	return resolved, nil
}

// sharesOut describes a domain's shares for the API.
func sharesOut(d libvirt.Domain) []codegen.Share {
	out := make([]codegen.Share, 0, len(d.Shares))
	for _, sh := range d.Shares {
		out = append(out, codegen.Share{Source: sh.Source, Tag: sh.Tag})
	}
	return out
}

// SetShares gives a VM exactly the folders asked for, at its next start. The
// first one also turns on the shared memory virtiofs needs.
func (s *VMs) SetShares(id uuid.UUID, wanted []codegen.Share) (codegen.VM, error) {
	if len(wanted) > maxShares {
		return codegen.VM{}, fmt.Errorf("%w: at most %d shared folders", ErrInvalid, maxShares)
	}

	// everything asked is checked before anything changes
	want := make([]libvirt.Share, 0, len(wanted))
	for _, w := range wanted {
		if !libvirt.ValidTag(w.Tag) {
			return codegen.VM{}, fmt.Errorf("%w: the tag %q is not 1-36 letters, digits, '.', '_' or '-'", ErrInvalid, w.Tag)
		}
		folder, err := ValidateShareFolder(w.Source)
		if err != nil {
			return codegen.VM{}, err
		}
		for _, other := range want {
			if other.Tag == w.Tag {
				return codegen.VM{}, fmt.Errorf("%w: the tag %s is given twice", ErrInvalid, w.Tag)
			}
			if other.Source == folder {
				return codegen.VM{}, fmt.Errorf("%w: %s is shared twice", ErrInvalid, folder)
			}
		}
		want = append(want, libvirt.Share{Source: folder, Tag: w.Tag})
	}

	l, dom, err := s.lookup(id)
	if err != nil {
		return codegen.VM{}, err
	}

	state, err := s.state(l, dom)
	if err != nil {
		return codegen.VM{}, err
	}
	if state == libvirt.StateCreating {
		return codegen.VM{}, fmt.Errorf("%w: the VM is still being created", ErrConflict)
	}

	desc, err := l.DomainGetXMLDesc(dom, golibvirt.DomainXMLInactive|golibvirt.DomainXMLSecure)
	if err != nil {
		return codegen.VM{}, err
	}

	d, err := libvirt.ParseDomainXML(desc)
	if err != nil {
		return codegen.VM{}, err
	}

	if d.Metadata == nil {
		return codegen.VM{}, fmt.Errorf("%w: CasaOS did not create this VM, so it does not change its devices", ErrConflict)
	}

	adding := false
	for _, sh := range want {
		if !slices.Contains(d.Shares, sh) {
			adding = true
		}
	}

	binary := ""
	if adding {
		if binary = virtiofsd(); binary == "" {
			return codegen.VM{}, fmt.Errorf("%w: this machine has no virtiofsd; install it with: sudo apt install virtiofsd", ErrConflict)
		}
	}

	// One definition with the shares and the shared memory they need: the
	// change is whole or not at all, and reaches the guest at its next start,
	// since virtiofs needs the shared memory from the start.
	next, changed, err := libvirt.WithShares(desc, want, binary)
	if errors.Is(err, libvirt.ErrMemoryBacking) {
		return codegen.VM{}, fmt.Errorf("%w: %s, which virtiofs cannot share", ErrConflict, err)
	}
	if err != nil {
		return codegen.VM{}, fmt.Errorf("read the VM's definition: %w", err)
	}

	if changed {
		if _, err := l.DomainDefineXMLFlags(next, golibvirt.DomainDefineValidate); err != nil {
			return codegen.VM{}, fmt.Errorf("share the folders: %w", err)
		}
		logger.Info("set the shared folders", zap.String("vm", d.Name), zap.Int("shares", len(want)))
	}

	return s.toVM(l, dom)
}
