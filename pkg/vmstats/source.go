package vmstats

import (
	"sync"

	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/google/uuid"
)

// connSource reads the live connection. Its StatsReader learns per connection
// whether the driver has ConnectGetAllDomainStats; a new connection resets it.
type connSource struct {
	conn   *libvirt.Conn
	reader libvirt.StatsReader

	mu   sync.Mutex
	last *golibvirt.Libvirt
}

// NewConnSource reads conn: all domains with one call (calls per domain on a
// driver without it), and a domain's title and description.
func NewConnSource(conn *libvirt.Conn) Source {
	s := &connSource{conn: conn}
	s.reader.OnFallback = func(err error) {
		logError("libvirt has no call for all domain statistics, reading each domain", err)
	}
	s.reader.OnNoWaitRefused = func(err error) {
		logError("libvirt refuses to skip busy domains when reading statistics", err)
	}
	return s
}

func (s *connSource) Snapshot() ([]libvirt.DomainStats, error) {
	l, err := s.conn.Get()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if l != s.last {
		s.reader.Reset()
		s.last = l
	}
	s.mu.Unlock()
	return s.reader.Read(l)
}

func (s *connSource) Describe(id string) (string, string, error) {
	l, err := s.conn.Get()
	if err != nil {
		return "", "", err
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return "", "", err
	}
	dom, err := l.DomainLookupByUUID(golibvirt.UUID(u))
	if err != nil {
		return "", "", err
	}
	desc, err := l.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return "", "", err
	}
	return describeXML(desc)
}

// describeXML is the title (else the name) and the detail of a domain: its
// description, else the OS CasaOS noted for it, else nothing.
func describeXML(desc string) (title, detail string, err error) {
	d, err := libvirt.ParseDomainXML(desc)
	if err != nil {
		return "", "", err
	}
	title = d.Title
	if title == "" {
		title = d.Name
	}
	detail = d.Description
	if detail == "" && d.Metadata != nil {
		detail = d.Metadata.OS
	}
	return title, detail, nil
}
