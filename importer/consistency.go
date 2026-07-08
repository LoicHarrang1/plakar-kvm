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
	// overlay; after the read the overlay is committed back. See
	// importDisksSnapshot.
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
