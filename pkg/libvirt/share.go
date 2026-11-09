package libvirt

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// Share is a host folder given to a VM over virtiofs: the guest mounts it by
// its tag.
type Share struct {
	Source string // the host folder
	Tag    string
}

// shareTag is what virtio-fs accepts as a tag: at most 36 bytes. Letters,
// digits and . _ - only, so it can be typed in an fstab line as it is.
var shareTag = regexp.MustCompile(`^[A-Za-z0-9._-]{1,36}$`)

// ValidTag reports whether tag can name a share.
func ValidTag(tag string) bool { return shareTag.MatchString(tag) }

// VirtiofsdPaths are where Debian and Ubuntu install virtiofsd: the Rust one
// of the virtiofsd package, then the C one Ubuntu 22.04 ships with QEMU.
var VirtiofsdPaths = []string{"/usr/libexec/virtiofsd", "/usr/lib/qemu/virtiofsd"}

// Virtiofsd returns the virtiofsd binary of the host; empty when there is none.
func Virtiofsd() string {
	for _, path := range VirtiofsdPaths {
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return ""
}

type filesystemXML struct {
	XMLName    xml.Name     `xml:"filesystem"`
	Type       string       `xml:"type,attr"`
	AccessMode string       `xml:"accessmode,attr,omitempty"`
	Driver     *fsDriverXML `xml:"driver"`
	Binary     *fsBinaryXML `xml:"binary"`
	Source     *fsSourceXML `xml:"source"`
	Target     *fsTargetXML `xml:"target"`
}

type fsDriverXML struct {
	Type string `xml:"type,attr"`
}

type fsBinaryXML struct {
	Path string `xml:"path,attr"`
}

type fsSourceXML struct {
	Dir string `xml:"dir,attr"`
}

type fsTargetXML struct {
	Dir string `xml:"dir,attr"`
}

// BuildShareXML writes the <filesystem> device of a share. virtiofsd is named
// explicitly: libvirt otherwise looks for a vhost-user descriptor, which
// Ubuntu 22.04's QEMU build does not install for its virtiofsd.
func BuildShareXML(s Share, virtiofsd string) (string, error) {
	if !ValidTag(s.Tag) {
		return "", fmt.Errorf("%q is not a share tag", s.Tag)
	}

	fs := filesystemXML{
		Type:       "mount",
		AccessMode: "passthrough",
		Driver:     &fsDriverXML{Type: "virtiofs"},
		Source:     &fsSourceXML{Dir: s.Source},
		Target:     &fsTargetXML{Dir: s.Tag},
	}
	if virtiofsd != "" {
		fs.Binary = &fsBinaryXML{Path: virtiofsd}
	}

	out, err := xml.Marshal(fs)
	return string(out), err
}

// sharesOf reads the virtiofs shares of a domain's filesystems; other
// filesystems (9p, …) are not shares CasaOS describes.
func sharesOf(filesystems []filesystemXML) []Share {
	var out []Share
	for _, fs := range filesystems {
		if fs.Type != "" && fs.Type != "mount" {
			continue
		}
		if fs.Driver == nil || fs.Driver.Type != "virtiofs" || fs.Source == nil || fs.Target == nil {
			continue
		}
		out = append(out, Share{Source: fs.Source.Dir, Tag: fs.Target.Dir})
	}
	return out
}

type memoryBackingXML struct {
	Source *struct {
		Type string `xml:"type,attr"`
	} `xml:"source"`
	Access *struct {
		Mode string `xml:"mode,attr"`
	} `xml:"access"`
}

// sharedMemory reports whether a <memoryBacking> lets virtiofsd see the
// guest's memory: any source shared with access mode shared.
func (m *memoryBackingXML) shared() bool {
	return m != nil && m.Access != nil && m.Access.Mode == "shared" && m.Source != nil &&
		(m.Source.Type == "memfd" || m.Source.Type == "file")
}

// ErrMemoryBacking is returned for a domain whose memory is set up some other
// way (hugepages, …): CasaOS does not rewrite what someone chose.
var ErrMemoryBacking = errors.New("the VM keeps its memory in a way of its own")

const sharedMemoryXML = `<memoryBacking><source type="memfd"/><access mode="shared"/></memoryBacking>`

// WithSharedMemory returns a domain's XML with the memory backing virtiofs
// needs, and whether it changed anything. Everything else is kept byte for
// byte: the element is spliced in after <currentMemory>, or <memory>, rather
// than the document being re-encoded, which would lose what the service does
// not model and mangle the casaos: metadata prefix.
func WithSharedMemory(domainXML string) (string, bool, error) {
	dec := xml.NewDecoder(bytes.NewReader([]byte(domainXML)))
	depth := 0
	insertAt := int64(-1)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false, err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 && t.Name.Local == "memoryBacking" {
				var m memoryBackingXML
				if err := dec.DecodeElement(&m, &t); err != nil {
					return "", false, err
				}
				if m.shared() {
					return domainXML, false, nil
				}
				return "", false, ErrMemoryBacking
			}
		case xml.EndElement:
			if depth == 2 && (t.Name.Local == "currentMemory" || (t.Name.Local == "memory" && insertAt < 0)) {
				insertAt = dec.InputOffset()
			}
			depth--
		}
	}

	if insertAt < 0 {
		return "", false, errors.New("the domain has no <memory>")
	}

	return domainXML[:insertAt] + sharedMemoryXML + domainXML[insertAt:], true, nil
}

// WithShares returns a domain's XML with exactly the virtiofs shares in want,
// and whether it changed anything: the shares not wanted are cut out, the new
// ones added at the end of <devices>, those kept left as libvirt wrote them.
// With any share the shared memory virtiofs needs is turned on too, so the
// whole change is one definition. Everything else is kept byte for byte, as
// in WithSharedMemory.
func WithShares(domainXML string, want []Share, virtiofsd string) (string, bool, error) {
	type cut struct{ from, to int64 }

	dec := xml.NewDecoder(bytes.NewReader([]byte(domainXML)))
	depth := 0
	devicesEnd := int64(-1)
	var cuts []cut
	var have []Share

	for {
		before := dec.InputOffset()
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", false, err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 3 && t.Name.Local == "filesystem" {
				var fs filesystemXML
				if err := dec.DecodeElement(&fs, &t); err != nil {
					return "", false, err
				}
				depth--

				shares := sharesOf([]filesystemXML{fs})
				if len(shares) == 0 {
					continue // not a virtiofs share: not ours to touch
				}
				if slices.Contains(want, shares[0]) && !slices.Contains(have, shares[0]) {
					have = append(have, shares[0])
					continue
				}
				cuts = append(cuts, cut{before, dec.InputOffset()})
			}
		case xml.EndElement:
			if depth == 2 && t.Name.Local == "devices" {
				devicesEnd = before
			}
			depth--
		}
	}

	if devicesEnd < 0 {
		return "", false, errors.New("the domain has no <devices>")
	}

	var added strings.Builder
	for _, sh := range want {
		if slices.Contains(have, sh) {
			continue
		}
		dev, err := BuildShareXML(sh, virtiofsd)
		if err != nil {
			return "", false, err
		}
		added.WriteString(dev)
	}

	if len(cuts) == 0 && added.Len() == 0 {
		return domainXML, false, nil
	}

	// cut from the end, so the offsets before stay right
	out := domainXML[:devicesEnd] + added.String() + domainXML[devicesEnd:]
	for i := len(cuts) - 1; i >= 0; i-- {
		out = out[:cuts[i].from] + out[cuts[i].to:]
	}

	if len(want) > 0 {
		shared, _, err := WithSharedMemory(out)
		if err != nil {
			return "", false, err
		}
		out = shared
	}

	return out, true, nil
}
