package exporter

import (
	"context"
	"fmt"
	pathpkg "path"
	"strings"

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/PlakarKorp/kloset/location"
	"golang.org/x/sync/errgroup"
)

// restoreRoot is the fixed, dedicated directory (on the target hypervisor) where
// snapshots are restored. Using a dedicated area avoids ever clobbering live VM
// images or DRBD devices: the restore lands as plain files that the admin then
// reviews and puts back into service explicitly.
const restoreRoot = "/var/lib/libvirt/images/plakar-restore"

func init() {
	exporter.Register("kvm", 0, NewExporter)
}

// Exporter restores a KVM snapshot (domain XML + disk images) as files under
// restoreRoot on the target, locally or over SSH. It deliberately does NOT
// redefine or start the domain, and never writes to DRBD/production devices.
type Exporter struct {
	access         writeAccess
	origin         string
	maxConcurrency int
}

func NewExporter(ctx context.Context, opts *connectors.Options, name string, config map[string]string) (exporter.Exporter, error) {
	connectURI, err := resolveConnectURI(config["location"])
	if err != nil {
		return nil, err
	}
	access, err := newWriteAccess(connectURI)
	if err != nil {
		return nil, err
	}
	mc := opts.MaxConcurrency
	if mc < 1 {
		mc = 1
	}
	return &Exporter{access: access, origin: opts.Hostname, maxConcurrency: mc}, nil
}

func (e *Exporter) Root() string          { return restoreRoot }
func (e *Exporter) Origin() string        { return e.origin }
func (e *Exporter) Type() string          { return "kvm" }
func (e *Exporter) Flags() location.Flags { return 0 }

func (e *Exporter) Ping(ctx context.Context) error  { return e.access.ping(ctx) }
func (e *Exporter) Close(ctx context.Context) error { return nil }

// Export writes each record (domain XML, disk image, and their directories)
// under restoreRoot, preserving the /<domain>/... layout. Mirrors the reference
// filesystem exporter: directories are created inline, regular files are written
// concurrently, and reader lifecycle is left to the SDK (no manual Close).
func (e *Exporter) Export(ctx context.Context, records <-chan *connectors.Record, results chan<- *connectors.Result) (ret error) {
	defer close(results)

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(e.maxConcurrency)

loop:
	for {
		select {
		case <-ctx.Done():
			ret = ctx.Err()
			break loop

		case record, ok := <-records:
			if !ok {
				break loop
			}

			if record.Err != nil || record.IsXattr {
				results <- record.Ok()
				continue
			}

			target := pathpkg.Join(restoreRoot, record.Pathname)
			if !within(restoreRoot, target) {
				results <- record.Error(fmt.Errorf("path %q escapes restore root", record.Pathname))
				continue
			}

			if record.FileInfo.Lmode.IsDir() {
				if err := e.access.mkdirAll(ctx, target); err != nil {
					results <- record.Error(err)
				} else {
					results <- record.Ok()
				}
				continue
			}

			g.Go(func() error {
				var err error
				if record.FileInfo.Lmode.IsRegular() {
					err = e.access.writeFile(ctx, target, record.Reader)
				}
				if err != nil {
					results <- record.Error(err)
				} else {
					results <- record.Ok()
				}
				return nil
			})
		}
	}

	if err := g.Wait(); err != nil && ret == nil {
		ret = err
	}
	return ret
}

// within reports whether p is inside root (or is root). POSIX paths (the target
// is always a Linux host).
func within(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+"/")
}
