package importer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/PlakarKorp/kloset/location"
	"github.com/PlakarKorp/kloset/objects"
)

func init() {
	importer.Register("kvm", 0, NewImporter)
}

// Importer backs up KVM/libvirt domains — their XML definition plus their disk
// images — into a Kloset repository.
//
// It has a single, opinionated behaviour (no tunables):
//   - Running domain  -> atomic external disk-only snapshot, crash-consistent
//     (the base becomes read-only and is streamed directly; the overlay is
//     merged back with blockcommit once the read completes). No guest agent is
//     involved, so nothing can wedge it.
//   - Stopped domain  -> the disk is already consistent and is read directly.
//
// Works locally (plugin on the KVM/DRBD node) or remotely over SSH (plugin
// elsewhere; libvirt control via qemu+ssh://, disk data via ssh). DRBD block
// devices on the Primary node are read like any other source.
type Importer struct {
	virsh        *virsh
	access       storageAccess
	origin       string
	domainFilter map[string]bool // nil means "all domains"

	sessMu   sync.Mutex
	sessions []*snapshotSession // in-flight snapshot sessions
}

// NewImporter builds a KVM importer from the connector configuration. Only two
// keys are accepted (see importer/schema.json): the required "location" and the
// optional "domains" filter.
func NewImporter(appCtx context.Context, opts *connectors.Options, name string, config map[string]string) (importer.Importer, error) {
	connectURI, err := resolveConnectURI(config["location"])
	if err != nil {
		return nil, err
	}

	var filter map[string]bool
	if raw := strings.TrimSpace(config["domains"]); raw != "" {
		filter = make(map[string]bool)
		for _, d := range strings.Split(raw, ",") {
			if d = strings.TrimSpace(d); d != "" {
				filter[d] = true
			}
		}
	}

	access, err := newAccess(connectURI)
	if err != nil {
		return nil, err
	}

	v := &virsh{connectURI: connectURI}

	origin := opts.Hostname
	if h, err := v.hostname(appCtx); err == nil && h != "" {
		origin = h
	}

	return &Importer{
		virsh:        v,
		access:       access,
		origin:       origin,
		domainFilter: filter,
	}, nil
}

// resolveConnectURI turns the kvm:// location into a libvirt connection URI
// understood by virsh -c. The location carries everything needed, including the
// SSH user for remote access:
//
//	kvm:///system                 -> qemu:///system                 (local)
//	kvm://host/system             -> qemu+ssh://host/system         (remote)
//	kvm://root@host/system        -> qemu+ssh://root@host/system    (remote, user)
func resolveConnectURI(loc string) (string, error) {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return "", fmt.Errorf("missing required 'location'")
	}

	parsed, err := url.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("invalid location %q: %w", loc, err)
	}
	if parsed.Scheme != "kvm" {
		return "", fmt.Errorf("location must use the kvm:// scheme, got %q", loc)
	}

	transport := strings.Trim(parsed.Path, "/")
	if transport != "system" && transport != "session" {
		return "", fmt.Errorf("location path must be /system or /session, got %q", parsed.Path)
	}

	if parsed.Host == "" {
		return fmt.Sprintf("qemu:///%s", transport), nil
	}

	userinfo := ""
	if parsed.User != nil && parsed.User.Username() != "" {
		userinfo = parsed.User.Username() + "@"
	}
	return fmt.Sprintf("qemu+ssh://%s%s/%s", userinfo, parsed.Host, transport), nil
}

func (p *Importer) Origin() string        { return p.origin }
func (p *Importer) Type() string          { return "kvm" }
func (p *Importer) Root() string          { return "/" }
func (p *Importer) Flags() location.Flags { return 0 }

func (p *Importer) Ping(ctx context.Context) error {
	return p.virsh.ping(ctx)
}

// Close is a safety net: if any snapshot session still has outstanding readers
// (e.g. a disk was never read), commit and clean it up so overlays don't linger.
func (p *Importer) Close(ctx context.Context) error {
	p.sessMu.Lock()
	pending := append([]*snapshotSession{}, p.sessions...)
	p.sessMu.Unlock()

	for _, s := range pending {
		s.mu.Lock()
		already := s.done
		if !already {
			s.done = true
		}
		s.mu.Unlock()
		if !already {
			s.commit()
		}
	}
	return nil
}

func (p *Importer) rememberSession(s *snapshotSession) {
	p.sessMu.Lock()
	p.sessions = append(p.sessions, s)
	p.sessMu.Unlock()
}

func (p *Importer) forgetSession(s *snapshotSession) {
	p.sessMu.Lock()
	defer p.sessMu.Unlock()
	for i, x := range p.sessions {
		if x == s {
			p.sessions = append(p.sessions[:i], p.sessions[i+1:]...)
			break
		}
	}
}

// Import walks the selected domains and streams one record per domain XML plus
// one record per disk image.
func (p *Importer) Import(ctx context.Context, records chan<- *connectors.Record, results <-chan *connectors.Result) error {
	defer close(records)

	domains, err := p.virsh.listDomains(ctx)
	if err != nil {
		return fmt.Errorf("listing domains: %w", err)
	}

	for _, domain := range domains {
		if p.domainFilter != nil && !p.domainFilter[domain] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		p.importDomain(ctx, domain, records)
	}
	return nil
}

func (p *Importer) importDomain(ctx context.Context, domain string, records chan<- *connectors.Record) {
	// 1. Domain definition.
	xmlPath := vmPath(domain, "domain.xml")
	xml, err := p.virsh.dumpXML(ctx, domain)
	if err != nil {
		records <- connectors.NewError(xmlPath, err)
		return
	}
	records <- connectors.NewRecord(xmlPath, "", memFileInfo("domain.xml", int64(len(xml))), nil,
		func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(xml)), nil })

	// 2. Disks.
	disks, err := p.virsh.listDisks(ctx, domain)
	if err != nil {
		records <- connectors.NewError(vmPath(domain, "disks"), err)
		return
	}

	running, err := p.virsh.isRunning(ctx, domain)
	if err != nil {
		records <- connectors.NewError(vmPath(domain), err)
		return
	}

	if running {
		// Consistent point-in-time via external snapshot.
		p.importDisksSnapshot(ctx, domain, disks, records)
	} else {
		// Already consistent: read the images directly.
		p.importDisksDirect(ctx, domain, disks, records)
	}
}

// importDisksDirect emits lazy readers straight over the source images. Used for
// stopped domains (already consistent). Works locally (os.Open) or remotely
// (ssh cat) via the storageAccess abstraction.
func (p *Importer) importDisksDirect(ctx context.Context, domain string, disks []diskInfo, records chan<- *connectors.Record) {
	for _, d := range disks {
		diskPath := vmPath(domain, "disks", path.Base(d.Source))
		src := d.Source

		fi, err := p.access.stat(ctx, src)
		if err != nil {
			records <- connectors.NewError(diskPath, err)
			continue
		}

		records <- connectors.NewRecord(diskPath, "", fi, nil,
			// context.Background(): the read may outlive Import; the record
			// Close (or a plakar-side abort) tears the reader down.
			func() (io.ReadCloser, error) { return p.access.open(context.Background(), src) })
	}
}

// memFileInfo builds a FileInfo for in-memory content (e.g. the domain XML).
func memFileInfo(name string, size int64) objects.FileInfo {
	return objects.NewFileInfo(name, size, 0o644, time.Now(), 0, 0, 0, 0, 1)
}

// vmPath builds an absolute snapshot path (under Root() == "/"). Record paths
// must be absolute and rooted at the importer's Root(), otherwise Plakar stores
// the objects but does not attach them to the browsable tree.
func vmPath(elem ...string) string {
	return path.Join(append([]string{"/"}, elem...)...)
}
