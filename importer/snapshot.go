package importer

import (
	"context"
	"fmt"
	"io"
	"log"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/PlakarKorp/kloset/connectors"
)

// overlayDir is where external snapshot overlays are created on the hypervisor.
// It is the standard libvirt images directory, writable by the QEMU process.
const overlayDir = "/var/lib/libvirt/images"

// importDisksSnapshot backs up the disks of a running domain via an atomic
// external disk-only snapshot — a consistent point-in-time with no staging copy.
//
// Lifecycle per domain:
//  1. snapshot-create-as --disk-only --atomic --no-metadata: the guest instantly
//     switches to a fresh qcow2 overlay while every original disk becomes a
//     read-only, frozen backing. Crash-consistent (no guest agent involved).
//  2. Stream each frozen backing (the original source, file or DRBD block device)
//     straight into Plakar. The guest keeps running on the overlay during the read.
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
	args, overlays, targets := buildSnapshotArgs(domain, snapname, disks, overlayDir, ts)

	// Clear any stale file left at a target overlay path by an interrupted run,
	// otherwise libvirt refuses with "external snapshot file already exists".
	// Safe: these are the paths we are about to create, and the guest is back on
	// its base disk (recoverOrphanOverlays ran first).
	for _, o := range overlays {
		_ = p.access.removeTemp(ctx, o)
	}

	if _, err := p.virsh.run(ctx, args...); err != nil {
		records <- connectors.NewError(vmPath(domain), fmt.Errorf("snapshot-create-as: %w", err))
		return
	}

	// Capture each disk's original base (pre-snapshot source), aligned with
	// targets, so blockcommit can be told the base explicitly — libvirt can't
	// auto-detect a raw block-device base (e.g. DRBD) in the chain.
	bases := make([]string, len(disks))
	for i := range disks {
		bases[i] = disks[i].Source
	}

	sess := &snapshotSession{
		imp:       p,
		domain:    domain,
		targets:   targets,
		overlays:  overlays,
		bases:     bases,
		remaining: len(disks),
	}
	p.rememberSession(sess)

	for i := range disks {
		d := disks[i]
		diskPath := vmPath(domain, "disks", path.Base(d.Source))
		base := d.Source // now a read-only, frozen backing

		// Each disk releases the session exactly once, whatever happens.
		var relOnce sync.Once
		release := func() { relOnce.Do(sess.release) }

		// Safety net: never back up a leftover overlay — that would store a partial
		// delta instead of the disk. recoverOrphanOverlays should have collapsed it
		// first; if not, fail loudly rather than silently store garbage.
		if isPlakarOverlay(domain, base) {
			records <- connectors.NewError(diskPath, fmt.Errorf(
				"disk %s is on a leftover plakar overlay %q; recover with: virsh blockcommit %s %s --active --pivot --wait",
				d.Target, base, domain, d.Target))
			release()
			continue
		}

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

// isPlakarOverlay reports whether a disk source is one of our leftover snapshot
// overlays (named "<domain>-<target>-plakar-<ts>.qcow2"). Such a source means a
// previous backup was interrupted before its cleanup ran.
func isPlakarOverlay(domain, source string) bool {
	b := path.Base(source)
	return strings.HasPrefix(b, domain+"-") && strings.Contains(b, "-plakar-") && strings.HasSuffix(b, ".qcow2")
}

// recoverOrphanOverlays is the crash guard rail: before a backup, if a disk is
// still on a leftover overlay (a previous run was killed before cleaning up), it
// merges the overlay back into the base (pivoting the guest onto its raw disk)
// and deletes the overlay file. Best-effort — failures are logged, not fatal.
func (p *Importer) recoverOrphanOverlays(ctx context.Context, domain string, disks []diskInfo) {
	for _, d := range disks {
		if !isPlakarOverlay(domain, d.Source) {
			continue
		}
		log.Printf("[kvm] %s: disk %s is on a leftover overlay %s from an interrupted backup; reverting to base", domain, d.Target, d.Source)
		if _, err := p.virsh.run(ctx, "blockcommit", domain, d.Target, "--active", "--pivot", "--wait"); err != nil {
			log.Printf("[kvm] recovery blockcommit %s/%s failed: %v", domain, d.Target, err)
			continue // do NOT delete the overlay: the guest still uses it
		}
		// Guest is back on its base disk; the leftover overlay is unused -> delete.
		if err := p.access.removeTemp(ctx, d.Source); err != nil {
			log.Printf("[kvm] removing leftover overlay %s failed: %v", d.Source, err)
		}
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
	bases    []string // original disk source per target (blockcommit --base)

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

// commit merges each overlay back into its base (pivoting the guest onto its
// original raw/qcow2 disk) and then deletes the now-unused overlay file.
//
// The delete happens ONLY when blockcommit succeeded: while the guest still runs
// on an overlay, deleting it would corrupt the VM. targets[i] and overlays[i] are
// parallel (see buildSnapshotArgs).
func (s *snapshotSession) commit() {
	ctx := context.Background()
	for i, t := range s.targets {
		overlay := s.overlays[i]

		// Tell blockcommit the base explicitly: libvirt can't auto-detect a raw
		// block-device base (DRBD) in the chain ("could not find base image").
		args := []string{"blockcommit", s.domain, t, "--active", "--pivot", "--wait"}
		if i < len(s.bases) && s.bases[i] != "" {
			args = append(args, "--base", s.bases[i])
		}

		if _, err := s.imp.virsh.run(ctx, args...); err != nil {
			log.Printf("[kvm] blockcommit %s/%s failed: %v — keeping overlay %s (guest may still use it)", s.domain, t, err, overlay)
			continue
		}
		// Guest is back on its base disk; the overlay is now unused -> delete it.
		if err := s.imp.access.removeTemp(ctx, overlay); err != nil {
			log.Printf("[kvm] removing overlay %s failed: %v", overlay, err)
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
