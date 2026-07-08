# KVM / DRBD Integration

## Overview

**KVM** (Kernel-based Virtual Machine), driven through **libvirt**, is the standard
Linux hypervisor stack. **DRBD** (Distributed Replicated Block Device) provides
network RAID-1 block replication and is commonly used as the storage layer under
KVM virtual machines for high availability.

This integration lets Plakar back up KVM/libvirt domains into a Kloset
repository:

- **Domain definitions** — the libvirt XML for every virtual machine (hardware
  layout, disk topology, network, etc.), i.e. everything needed to redefine the VM.
- **Disk images** — the backing storage of each domain, whether it is a
  file-backed image (`qcow2`, `raw`) or a **DRBD block device** (`/dev/drbdN`).
  On the DRBD **Primary** node these devices are read like any other block device.

Following the same philosophy as the Plakar Proxmox integration (which wraps
`vzdump`), this connector wraps the native `virsh` and `qemu-img` tools rather
than linking the libvirt C bindings. This keeps the plugin CGO-free and avoids
GPL/LGPL dependencies.

## Requirements

- The plugin runs on (or has access to) a KVM host with `virsh` and `qemu-img`
  in `PATH`.
- For **application-consistent** backups (`consistency: fsfreeze`), the
  `qemu-guest-agent` must be installed and running inside the guests.
- For DRBD-backed VMs, run the backup on the **Primary** node.

> **Local mode** (plugin on the KVM/DRBD node) is implemented today. Remote mode
> via `qemu+ssh://` is derived from the location URI but requires the disk images
> to be reachable from where the plugin runs — see the Roadmap.

## Configuration

The configuration parameters are as follows:

- `location` (required): `kvm://[<host>]/(system|session)`
  - `kvm:///system` → `qemu:///system` (local)
  - `kvm://node1/system` → `qemu+ssh://node1/system` (remote)
- `connect_uri` (optional): explicit libvirt URI passed to `virsh -c`, overriding
  the one derived from `location` (e.g. `qemu+ssh://root@node1/system`).
- `domains` (optional): comma-separated VM names to include. Empty = all domains.
- `consistency` (optional, default `fsfreeze`):
  - `crash` — read live images, no coordination (crash-consistent).
  - `fsfreeze` — quiesce the guest via qemu-guest-agent, copy each disk with
    `qemu-img convert`, then thaw (application-consistent).
  - `snapshot` — atomic external disk-only snapshot (see Roadmap; not yet implemented).
- `include_disks` (optional, default `true`): back up disk images in addition to
  the domain XML.

Stopped domains are always read directly (they are already consistent),
regardless of the `consistency` setting.

## Snapshot layout

Each domain produces the following tree inside the Kloset snapshot:

```
<domain>/domain.xml
<domain>/disks/<image-basename>[.qcow2]
```

## Examples

```bash
# configure a local KVM source (application-consistent)
$ plakar source add myKVM kvm:///system consistency=fsfreeze

# back up all VMs on the local hypervisor
$ plakar backup @myKVM

# back up only two VMs, definitions only (no disks)
$ plakar source add webVMs kvm:///system domains=web01,web02 include_disks=false
$ plakar backup @webVMs
```

## Building and installing

The plugin targets Linux/KVM hosts. On such a host:

```bash
# resolve dependencies (needs network access)
$ go mod tidy

# build the importer binary
$ make build            # produces ./kvmImporter

# package the plugin
$ plakar pkg create manifest.yaml v0.1.0
# -> kvm_v0.1.0_linux_amd64.ptar

# install and verify
$ plakar pkg add ./kvm_v0.1.0_linux_amd64.ptar
$ plakar pkg show
```

## Status / Roadmap

Implemented:
- Domain enumeration and XML export.
- Disk export in `crash` and `fsfreeze` (application-consistent) modes.
- DRBD block devices as sources on the Primary node.

Planned:
- `snapshot` mode: atomic external disk-only snapshot
  (`virsh snapshot-create-as --disk-only --atomic --no-metadata`) with
  `blockcommit --active --pivot` to avoid staging full disk copies.
- LVM-snapshot strategy for raw DRBD volumes (snapshot the DRBD backing LV under
  fsfreeze/fsthaw) for space-efficient consistent reads.
- Remote mode: stream disk data over SSH when the plugin does not run on the node.
- Incremental backups via QEMU dirty bitmaps.
- An **exporter** (restore) counterpart to redefine domains and restore disks.

## License

ISC — see [LICENSE](LICENSE).
