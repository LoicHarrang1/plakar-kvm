package importer

import (
	"context"
	"fmt"
	"log"
)

// consistencyMode controls how disk images are made consistent before reading.
type consistencyMode string

const (
	// modeCrash reads the live images with no coordination. Fast, but only
	// crash-consistent: the guest may have dirty pages/filesystem state not yet
	// flushed. Acceptable for journaled filesystems that can replay on restore.
	modeCrash consistencyMode = "crash"

	// modeFsfreeze quiesces the guest filesystems via the qemu-guest-agent
	// (fsFreeze) around the read window, then thaws them (fsThaw). This yields
	// application-consistent images when the agent is installed in the guest.
	modeFsfreeze consistencyMode = "fsfreeze"

	// modeSnapshot takes an atomic external disk-only snapshot so the base
	// image becomes read-only and frozen while the guest keeps writing to a new
	// overlay; after the read the overlay is committed back. See snapshotDisks.
	modeSnapshot consistencyMode = "snapshot"
)

func parseConsistency(s string) (consistencyMode, error) {
	switch consistencyMode(s) {
	case "":
		return modeFsfreeze, nil // default
	case modeCrash, modeFsfreeze, modeSnapshot:
		return consistencyMode(s), nil
	default:
		return "", fmt.Errorf("invalid consistency mode %q (want crash|fsfreeze|snapshot)", s)
	}
}

// freeze quiesces the guest filesystems. Best-effort: if the guest agent is
// missing we log and continue with a crash-consistent read rather than failing
// the whole backup.
func (p *Importer) freeze(ctx context.Context, domain string) (thaw func()) {
	if _, err := p.virsh.run(ctx, "domfsfreeze", domain); err != nil {
		log.Printf("[kvm] fsfreeze %s failed, falling back to crash-consistent read: %v", domain, err)
		return func() {}
	}
	return func() {
		if _, err := p.virsh.run(ctx, "domfsthaw", domain); err != nil {
			log.Printf("[kvm] fsthaw %s failed: %v", domain, err)
		}
	}
}

// snapshotDisks creates an atomic external disk-only snapshot of the domain and
// returns, for each target disk, the frozen base path to read plus a cleanup
// function that commits the live overlay back into the base and pivots.
//
// TODO(kvm): implement the full snapshot lifecycle:
//   - virsh snapshot-create-as <dom> plakar-<ts> \
//       --disk-only --atomic --no-metadata \
//       --diskspec <target>,snapshot=external,file=<overlay>
//   - read the now-static base image for each disk
//   - virsh blockcommit <dom> <target> --active --pivot --wait
//     to merge the overlay back and drop it
//
// This is the correct path for file-backed qcow2 disks. For raw DRBD block
// devices (/dev/drbdN) an overlay cannot live on the block device itself; the
// recommended consistent path there is an LVM snapshot of the DRBD backing
// volume (on the Primary node) wrapped in fsfreeze/fsthaw — tracked separately.
func (p *Importer) snapshotDisks(ctx context.Context, domain string, disks []diskInfo) (bases map[string]string, cleanup func(), err error) {
	return nil, nil, fmt.Errorf("consistency mode %q not yet implemented (use crash or fsfreeze for now)", modeSnapshot)
}
