package importer

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
)

// virsh is a thin wrapper around the libvirt `virsh` command line tool.
//
// We deliberately shell out to virsh (and qemu-img) instead of linking the
// libvirt C bindings: it keeps the plugin CGO-free and avoids pulling in
// LGPL/GPL dependencies, which the Plakar contribution guidelines discourage.
// This mirrors the approach of the Proxmox integration, which wraps the native
// vzdump tool rather than reimplementing it.
type virsh struct {
	connectURI string
}

// diskInfo describes a single block device attached to a domain, as reported
// by `virsh domblklist <domain> --details`.
type diskInfo struct {
	DevType string // "file" or "block"
	Device  string // "disk", "cdrom", ...
	Target  string // guest-visible target, e.g. "vda"
	Source  string // host path, e.g. /var/lib/libvirt/images/vm.qcow2 or /dev/drbd0
}

func (v *virsh) command(ctx context.Context, args ...string) *exec.Cmd {
	full := make([]string, 0, len(args)+2)
	if v.connectURI != "" {
		full = append(full, "-c", v.connectURI)
	}
	full = append(full, args...)
	return exec.CommandContext(ctx, "virsh", full...)
}

func (v *virsh) run(ctx context.Context, args ...string) ([]byte, error) {
	log.Printf("[kvm][virsh] -c %s %s", v.connectURI, strings.Join(args, " "))
	var stdout, stderr bytes.Buffer
	cmd := v.command(ctx, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("virsh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// ping verifies that virsh can reach the hypervisor.
func (v *virsh) ping(ctx context.Context) error {
	_, err := v.run(ctx, "hostname")
	return err
}

// hostname returns the hypervisor hostname (used as the snapshot Origin).
func (v *virsh) hostname(ctx context.Context) (string, error) {
	out, err := v.run(ctx, "hostname")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// listDomains returns the names of all defined domains (running or not).
func (v *virsh) listDomains(ctx context.Context) ([]string, error) {
	out, err := v.run(ctx, "list", "--all", "--name")
	if err != nil {
		return nil, err
	}
	return parseDomainList(out), nil
}

// parseDomainList extracts domain names from `virsh list --all --name` output.
func parseDomainList(out []byte) []string {
	var domains []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		name := strings.TrimSpace(scanner.Text())
		if name != "" {
			domains = append(domains, name)
		}
	}
	return domains
}

// isRunning reports whether the domain is currently running (relevant for the
// choice of consistency strategy: a stopped domain is already consistent).
func (v *virsh) isRunning(ctx context.Context, domain string) (bool, error) {
	out, err := v.run(ctx, "domstate", domain)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "running", nil
}

// dumpXML returns the libvirt domain definition (the authoritative metadata to
// restore a VM: hardware layout, disk topology, network, etc.).
func (v *virsh) dumpXML(ctx context.Context, domain string) ([]byte, error) {
	return v.run(ctx, "dumpxml", domain)
}

// listDisks parses `virsh domblklist <domain> --details`, skipping cdroms and
// devices without a backing source.
func (v *virsh) listDisks(ctx context.Context, domain string) ([]diskInfo, error) {
	out, err := v.run(ctx, "domblklist", domain, "--details")
	if err != nil {
		return nil, err
	}
	return parseDomblklist(out), nil
}

// parseDomblklist parses `virsh domblklist <domain> --details` output, skipping
// the header/separator rows, cdroms, and devices without a backing source.
func parseDomblklist(out []byte) []diskInfo {
	var disks []diskInfo
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		fields := strings.Fields(line)
		// header row: "Type Device Target Source"
		if len(fields) >= 1 && fields[0] == "Type" {
			continue
		}
		if len(fields) < 4 {
			continue
		}
		d := diskInfo{
			DevType: fields[0],
			Device:  fields[1],
			Target:  fields[2],
			Source:  fields[3],
		}
		if d.Device == "cdrom" || d.Source == "-" {
			continue
		}
		disks = append(disks, d)
	}
	return disks
}
