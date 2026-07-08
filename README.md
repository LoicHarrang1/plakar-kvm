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

- `virsh` in `PATH` on the machine running Plakar (it reaches the hypervisor
  locally or over `qemu+ssh://`).
- `qemu-img` available **where the disk images live** (locally in local mode, on
  the hypervisor in remote mode) — required by `consistency: fsfreeze`.
- For **application-consistent** backups (`consistency: fsfreeze`), the
  `qemu-guest-agent` must be installed and running inside the guests.
- For DRBD-backed VMs, target the **Primary** node (block devices are only
  readable there).

### Local vs remote mode

The connector works in two modes, selected automatically from the connection URI:

- **Local mode** (`kvm:///system`): the plugin runs on the KVM/DRBD node and
  reads/converts disk images directly on the local filesystem or block devices.
- **Remote mode** (`kvm://<host>/system`, i.e. `qemu+ssh://<host>/system`):
  Plakar runs elsewhere (e.g. a dedicated backup server) and does **not** need to
  be installed on the hypervisor. The libvirt control plane goes through
  `qemu+ssh://`, and the disk **data** is read/converted on the hypervisor and
  streamed back over SSH (`ssh cat`, remote `qemu-img convert`). This requires
  non-interactive SSH access (key-based, `BatchMode`) from the Plakar host to the
  hypervisor, as a user allowed to read the disk images / block devices (usually
  `root` for DRBD devices).

## Configuration

The configuration parameters are as follows:

- `location` (required): `kvm://[<host>]/(system|session)`
  - `kvm:///system` → `qemu:///system` (local)
  - `kvm://node1/system` → `qemu+ssh://node1/system` (remote)
- `connect_uri` (optional): explicit libvirt URI passed to `virsh -c`, overriding
  the one derived from `location` (e.g. `qemu+ssh://root@node1/system`).
- `ssh_user` (optional, remote mode): SSH user for reading disk data on the
  hypervisor. Defaults to the user in the connection URI.
- `ssh_port` (optional, remote mode): SSH port for reading disk data. Defaults to
  the URI port (or 22).
- `domains` (optional): comma-separated VM names to include. Empty = all domains.
- `consistency` (optional, default `fsfreeze`):
  - `crash` — read live images, no coordination (crash-consistent).
  - `fsfreeze` — quiesce the guest via qemu-guest-agent, copy each disk with
    `qemu-img convert` to a staging file, then thaw (application-consistent, needs
    staging space equal to the disks).
  - `snapshot` — atomic external disk-only snapshot: the guest instantly switches
    to a qcow2 overlay, the frozen base is streamed **directly** into Plakar (no
    staging copy), then `blockcommit --active --pivot` merges the overlay back.
    The guest is frozen only for the snapshot instant (with `--quiesce`). Best for
    large disks. Works for file-backed disks and raw DRBD block devices (the
    overlay is a file in `overlay_dir`; the DRBD device stays the frozen backing).
- `overlay_dir` (optional, default `/var/lib/libvirt/images`): where snapshot
  overlays are created on the hypervisor (`snapshot` mode). Must be writable by
  the QEMU/libvirt process.
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
# LOCAL: configure a local KVM source (application-consistent)
$ plakar source add myKVM kvm:///system consistency=fsfreeze

# back up all VMs on the local hypervisor
$ plakar backup @myKVM

# back up only two VMs, definitions only (no disks)
$ plakar source add webVMs kvm:///system domains=web01,web02 include_disks=false
$ plakar backup @webVMs

# REMOTE: back up VMs on a remote hypervisor over SSH (Plakar not installed there)
# requires key-based SSH from this host to root@FR-KVM-TEST1
$ plakar source add remoteKVM kvm://FR-KVM-TEST1/system consistency=fsfreeze
$ plakar backup @remoteKVM
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
- Disk export in `crash`, `fsfreeze` (application-consistent) and `snapshot`
  (external disk-only snapshot, no staging copy) modes.
- DRBD block devices as sources on the Primary node.
- Local and remote (SSH) modes — Plakar need not be installed on the hypervisor.

Planned:
- Incremental backups via QEMU dirty bitmaps.
- An **exporter** (restore) counterpart to redefine domains and restore disks.

## License

ISC — see [LICENSE](LICENSE).
