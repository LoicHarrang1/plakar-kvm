package importer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
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
type Importer struct {
	virsh        *virsh
	origin       string
	consistency  consistencyMode
	domainFilter map[string]bool // nil means "all domains"
	includeDisks bool
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

	var filter map[string]bool
	if raw := strings.TrimSpace(config["domains"]); raw != "" {
		filter = make(map[string]bool)
		for _, d := range strings.Split(raw, ",") {
			if d = strings.TrimSpace(d); d != "" {
				filter[d] = true
			}
		}
	}

	v := &virsh{connectURI: connectURI}

	origin := opts.Hostname
	if h, err := v.hostname(appCtx); err == nil && h != "" {
		origin = h
	}

	return &Importer{
		virsh:        v,
		origin:       origin,
		consistency:  consistency,
		domainFilter: filter,
		includeDisks: includeDisks,
	}, nil
}

// resolveConnectURI turns the kvm:// location (or an explicit connect_uri) into
// a libvirt connection URI understood by virsh -c.
//
//	kvm:///system          -> qemu:///system
//	kvm:///session         -> qemu:///session
//	kvm://node1/system     -> qemu+ssh://node1/system
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

func (p *Importer) Close(ctx context.Context) error {
	return nil
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
	xmlPath := path.Join(domain, "domain.xml")
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
		records <- connectors.NewError(path.Join(domain, "disks"), err)
		return
	}

	running, err := p.virsh.isRunning(ctx, domain)
	if err != nil {
		records <- connectors.NewError(domain, err)
		return
	}

	// A stopped domain is already consistent; skip quiescing.
	mode := p.consistency
	if !running {
		mode = modeCrash
	}

	switch mode {
	case modeSnapshot:
		bases, cleanup, err := p.snapshotDisks(ctx, domain, disks)
		if err != nil {
			records <- connectors.NewError(domain, err)
			return
		}
		defer cleanup()
		_ = bases // TODO(kvm): emit lazy readers over the frozen base images
	case modeFsfreeze:
		p.importDisksFsfreeze(ctx, domain, disks, records)
	default: // modeCrash
		p.importDisksCrash(ctx, domain, disks, records)
	}
}

// importDisksCrash emits lazy readers straight over the live source images
// (crash-consistent). Suitable for stopped domains or journaled guests.
func (p *Importer) importDisksCrash(ctx context.Context, domain string, disks []diskInfo, records chan<- *connectors.Record) {
	for _, d := range disks {
		diskPath := path.Join(domain, "disks", path.Base(d.Source))
		src := d.Source

		info, err := os.Stat(src)
		if err != nil {
			records <- connectors.NewError(diskPath, err)
			continue
		}
		fi := objects.FileInfoFromStat(info)

		records <- connectors.NewRecord(diskPath, "", fi, nil,
			func() (io.ReadCloser, error) { return os.Open(src) })
	}
}

// importDisksFsfreeze quiesces the guest, materialises a consistent, sparse
// qcow2 copy of every disk (via qemu-img convert), then thaws the guest. The
// emitted records read those temporary copies and delete them on Close.
//
// The freeze window is kept as short as possible but does span the qemu-img
// conversions so that all disks of a domain share a single point in time.
func (p *Importer) importDisksFsfreeze(ctx context.Context, domain string, disks []diskInfo, records chan<- *connectors.Record) {
	thaw := p.freeze(ctx, domain)

	type staged struct {
		path string // record path inside the snapshot
		tmp  string // temporary qcow2 file on disk
	}
	var ready []staged

	for _, d := range disks {
		tmp, err := os.CreateTemp("", "plakar-kvm-*.qcow2")
		if err != nil {
			records <- connectors.NewError(path.Join(domain, "disks", path.Base(d.Source)), err)
			continue
		}
		tmp.Close()

		// qemu-img convert produces a compact, self-contained qcow2 while the
		// guest filesystems are frozen -> application-consistent image.
		cmd := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "qcow2", d.Source, tmp.Name())
		if out, err := cmd.CombinedOutput(); err != nil {
			os.Remove(tmp.Name())
			records <- connectors.NewError(path.Join(domain, "disks", path.Base(d.Source)),
				fmt.Errorf("qemu-img convert: %w: %s", err, strings.TrimSpace(string(out))))
			continue
		}
		ready = append(ready, staged{
			path: path.Join(domain, "disks", path.Base(d.Source)+".qcow2"),
			tmp:  tmp.Name(),
		})
	}

	// Guest can resume as soon as the consistent copies exist.
	thaw()

	for _, s := range ready {
		info, err := os.Stat(s.tmp)
		if err != nil {
			os.Remove(s.tmp)
			records <- connectors.NewError(s.path, err)
			continue
		}
		fi := objects.FileInfoFromStat(info)
		tmpPath := s.tmp
		records <- connectors.NewRecord(s.path, "", fi, nil,
			func() (io.ReadCloser, error) { return newTempFileReader(tmpPath) })
	}
}

// memFileInfo builds a FileInfo for in-memory content (e.g. the domain XML).
func memFileInfo(name string, size int64) objects.FileInfo {
	return objects.NewFileInfo(name, size, 0o644, time.Now(), 0, 0, 0, 0, 1)
}

// tempFileReader reads a temporary file and removes it once closed, so staged
// disk copies do not accumulate on disk after ingestion.
type tempFileReader struct{ f *os.File }

func newTempFileReader(name string) (io.ReadCloser, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return &tempFileReader{f: f}, nil
}

func (r *tempFileReader) Read(p []byte) (int, error) { return r.f.Read(p) }

func (r *tempFileReader) Close() error {
	name := r.f.Name()
	err := r.f.Close()
	os.Remove(name)
	return err
}
