package importer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"strconv"
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

// Importer backs up KVM/libvirt domains (their XML definition and, optionally,
// their disk images) into a Kloset repository. Disks backed by DRBD block
// devices are supported: on the Primary node they appear as regular block
// devices and are read like any other source.
//
// It works both locally (plugin running on the KVM/DRBD node) and remotely
// (plugin running elsewhere, reaching the hypervisor over SSH); see storageAccess.
type Importer struct {
	virsh        *virsh
	access       storageAccess
	remote       bool
	origin       string
	consistency  consistencyMode
	domainFilter map[string]bool // nil means "all domains"
	includeDisks bool
	overlayDir   string // where external snapshot overlays are created (snapshot mode)

	sessMu   sync.Mutex
	sessions []*snapshotSession // in-flight snapshot sessions (snapshot mode)
}

// NewImporter builds a KVM importer from the connector configuration. See
// importer/schema.json for the accepted keys.
func NewImporter(appCtx context.Context, opts *connectors.Options, name string, config map[string]string) (importer.Importer, error) {
	connectURI, err := resolveConnectURI(config)
	if err != nil {
		return nil, err
	}

	consistency, err := parseConsistency(config["consistency"])
	if err != nil {
		return nil, err
	}

	includeDisks := true
	if v, ok := config["include_disks"]; ok {
		if includeDisks, err = strconv.ParseBool(v); err != nil {
			return nil, fmt.Errorf("invalid include_disks %q: %w", v, err)
		}
	}

	overlayDir := strings.TrimSpace(config["overlay_dir"])
	if overlayDir == "" {
		overlayDir = "/var/lib/libvirt/images"
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

	access, remote, err := newAccess(connectURI, config)
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
		remote:       remote,
		origin:       origin,
		consistency:  consistency,
		domainFilter: filter,
		includeDisks: includeDisks,
		overlayDir:   overlayDir,
	}, nil
}

// resolveConnectURI turns the kvm:// location (or an explicit connect_uri) into
// a libvirt connection URI understood by virsh -c.
//
//	kvm:///system          -> qemu:///system              (local)
//	kvm:///session         -> qemu:///session             (local)
//	kvm://node1/system     -> qemu+ssh://node1/system      (remote)
func resolveConnectURI(config map[string]string) (string, error) {
	if u := strings.TrimSpace(config["connect_uri"]); u != "" {
		return u, nil
	}

	loc := strings.TrimSpace(config["location"])
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
	return fmt.Sprintf("qemu+ssh://%s/%s", parsed.Host, transport), nil
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
// (optionally) one record per disk image.
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
	// 1. Domain definition (always).
	xmlPath := vmPath(domain, "domain.xml")
	xml, err := p.virsh.dumpXML(ctx, domain)
	if err != nil {
		records <- connectors.NewError(xmlPath, err)
		return
	}
	records <- connectors.NewRecord(xmlPath, "", memFileInfo("domain.xml", int64(len(xml))), nil,
		func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(xml)), nil })

	// 2. Disk images (optional).
	if !p.includeDisks {
		return
	}

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

	// A stopped domain is already consistent; skip quiescing.
	mode := p.consistency
	if !running {
		mode = modeCrash
	}

	switch mode {
	case modeSnapshot:
		p.importDisksSnapshot(ctx, domain, disks, records)
	case modeFsfreeze:
		p.importDisksFsfreeze(ctx, domain, disks, records)
	default: // modeCrash
		p.importDisksCrash(ctx, domain, disks, records)
	}
}

// importDisksCrash emits lazy readers straight over the live source images
// (crash-consistent). Suitable for stopped domains or journaled guests. Works
// locally (os.Open) or remotely (ssh cat) via the storageAccess abstraction.
func (p *Importer) importDisksCrash(ctx context.Context, domain string, disks []diskInfo, records chan<- *connectors.Record) {
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

// importDisksFsfreeze quiesces the guest, materialises a consistent, sparse
// qcow2 copy of every disk (via qemu-img convert, local or remote), then thaws
// the guest. The emitted records read those temporary copies and delete them on
// Close.
//
// The freeze window is kept as short as possible but does span the qemu-img
// conversions so that all disks of a domain share a single point in time.
func (p *Importer) importDisksFsfreeze(ctx context.Context, domain string, disks []diskInfo, records chan<- *connectors.Record) {
	thaw := p.freeze(ctx, domain)

	type staged struct {
		path string // record path inside the snapshot
		tmp  string // temporary qcow2 file (local path or remote path)
	}
	var ready []staged

	for _, d := range disks {
		tmp, err := p.access.convertToTemp(ctx, d.Source)
		if err != nil {
			records <- connectors.NewError(vmPath(domain, "disks", path.Base(d.Source)), err)
			continue
		}
		ready = append(ready, staged{
			path: vmPath(domain, "disks", path.Base(d.Source)+".qcow2"),
			tmp:  tmp,
		})
	}

	// Guest can resume as soon as the consistent copies exist.
	thaw()

	for _, s := range ready {
		fi, err := p.access.statTemp(ctx, s.tmp)
		if err != nil {
			_ = p.access.removeTemp(ctx, s.tmp)
			records <- connectors.NewError(s.path, err)
			continue
		}
		tmp := s.tmp
		records <- connectors.NewRecord(s.path, "", fi, nil,
			func() (io.ReadCloser, error) { return p.access.openTemp(context.Background(), tmp) })
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
