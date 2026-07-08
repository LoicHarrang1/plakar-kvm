package importer

import (
	"context"
	"fmt"
	"io"
	"log"
	"path"
	"sync"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
)

// importDisksSnapshot backs up disks using an atomic external disk-only snapshot
// instead of copying the full disk while frozen.
//
// Lifecycle per domain:
//  1. snapshot-create-as --disk-only --atomic --no-metadata (with --quiesce when
//     the guest agent is available): the guest instantly switches to a fresh
//     qcow2 overlay while every original disk becomes a read-only, frozen backing.
//  2. Stream each frozen backing (the original source, file or DRBD block device)
//     straight into Plakar — no staging copy. The guest keeps running on the
//     overlay during the read.
//  3. Once ALL disk readers of the domain have been consumed and closed,
//     blockcommit --active --pivot merges each overlay back into its base and the
//     overlay files are removed.
//
// Step 3 must not run before the reads finish, otherwise blockcommit would mutate
// a base that Plakar is still reading. This is enforced by refcounting reader
// Close() calls in snapshotSession.
func (p *Importer) importDisksSnapshot(ctx context.Context, domain string, disks []diskInfo, records chan<- *connectors.Record) {
	if len(disks) == 0 {
		return
	}

	ts := time.Now().Unix()
	snapname := fmt.Sprintf("plakar-%d", ts)
	args, overlays, targets := buildSnapshotArgs(domain, snapname, disks, p.overlayDir, ts)

	// Prefer an application-consistent (quiesced) snapshot; fall back to a
	// crash-consistent one if the guest agent is unavailable.
	quiesced := append(append([]string{}, args...), "--quiesce")
	if _, err := p.virsh.run(ctx, quiesced...); err != nil {
		log.Printf("[kvm] quiesced snapshot of %s failed (%v); retrying without --quiesce", domain, err)
		if _, err := p.virsh.run(ctx, args...); err != nil {
			records <- connectors.NewError(domain, fmt.Errorf("snapshot-create-as: %w", err))
			return
		}
	}

	sess := &snapshotSession{
		imp:       p,
		domain:    domain,
		targets:   targets,
		overlays:  overlays,
		remaining: len(disks),
	}
	p.rememberSession(sess)

	for i := range disks {
		d := disks[i]
		diskPath := path.Join(domain, "disks", path.Base(d.Source))
		base := d.Source // now a read-only, frozen backing

		// Each disk releases the session exactly once, whatever happens.
		var relOnce sync.Once
		release := func() { relOnce.Do(sess.release) }

		fi, err := p.access.stat(ctx, base)
		if err != nil {
			records <- connectors.NewError(diskPath, err)
			release()
			continue
		}

		records <- connectors.NewRecord(diskPath, "", fi, nil,
			func() (io.ReadCloser, error) {
				rc, err := p.access.open(context.Background(), base)
				if err != nil {
					release()
					return nil, err
				}
				return &sessionReader{ReadCloser: rc, release: release}, nil
			})
	}
}

// buildSnapshotArgs assembles the virsh snapshot-create-as invocation and the
// per-disk overlay/target bookkeeping. Split out so it can be unit tested.
func buildSnapshotArgs(domain, snapname string, disks []diskInfo, overlayDir string, ts int64) (args, overlays, targets []string) {
	args = []string{"snapshot-create-as", domain, snapname, "--disk-only", "--atomic", "--no-metadata"}
	for _, d := range disks {
		overlay := path.Join(overlayDir, fmt.Sprintf("%s-%s-plakar-%d.qcow2", domain, d.Target, ts))
		args = append(args, "--diskspec", fmt.Sprintf("%s,snapshot=external,file=%s", d.Target, overlay))
		overlays = append(overlays, overlay)
		targets = append(targets, d.Target)
	}
	return args, overlays, targets
}

// snapshotSession tracks the outstanding disk readers of one domain snapshot and
// merges/cleans up the overlays once the last reader is closed.
type snapshotSession struct {
	imp      *Importer
	domain   string
	targets  []string
	overlays []string

	mu        sync.Mutex
	remaining int
	done      bool
}

// release records that one disk is done being read; the last one triggers commit.
func (s *snapshotSession) release() {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.remaining--
	if s.remaining > 0 {
		s.mu.Unlock()
		return
	}
	s.done = true
	s.mu.Unlock()
	s.commit()
}

// commit merges every overlay back into its base and removes the overlay files.
func (s *snapshotSession) commit() {
	ctx := context.Background()
	for _, t := range s.targets {
		if _, err := s.imp.virsh.run(ctx, "blockcommit", s.domain, t, "--active", "--pivot", "--wait"); err != nil {
			log.Printf("[kvm] blockcommit %s/%s failed: %v (overlay may need manual cleanup)", s.domain, t, err)
		}
	}
	for _, o := range s.overlays {
		if err := s.imp.access.removeTemp(ctx, o); err != nil {
			log.Printf("[kvm] removing overlay %s failed: %v", o, err)
		}
	}
	s.imp.forgetSession(s)
}

// sessionReader releases its snapshot session when the disk stream is closed.
type sessionReader struct {
	io.ReadCloser
	release func()
}

func (r *sessionReader) Close() error {
	err := r.ReadCloser.Close()
	r.release()
	return err
}
