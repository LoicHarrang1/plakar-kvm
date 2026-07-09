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
`vzdump`), this connector wraps the native `virsh` tool rather than linking the
libvirt C bindings. This keeps the plugin CGO-free and avoids GPL/LGPL
dependencies.

## Behaviour (no tunables)

The connector has a single, opinionated behaviour — there is nothing to tune:

- **Running domain** → **atomic external disk-only snapshot**, crash-consistent.
  The base becomes read-only and is streamed directly (no staging copy); the
  overlay is merged back with `blockcommit` once the read completes. No guest
  agent is involved, so nothing can wedge it.
- **Stopped domain** → the disk is already consistent and is read directly.
- Disk streams are **SSH-compressed** (`-C`) in remote mode — a big win for raw
  disks full of zeros.

Local vs remote is selected automatically from the `location`:

- **Local** (`kvm:///system`): the plugin runs on the KVM/DRBD node; disks are
  read on the local filesystem / block devices.
- **Remote** (`kvm://[user@]host/system`): Plakar runs elsewhere and does **not**
  need to be installed on the hypervisor. libvirt control goes through
  `qemu+ssh://`; disk data is streamed back over SSH (`ssh cat`).

## Requirements

- **Plakar host**: `virsh` (libvirt-clients) and `ssh` in `PATH`.
- **Hypervisor**: `sshd`; the SSH user must be allowed to read the disk images /
  block devices (usually `root` for DRBD `/dev/drbdN`). Non-interactive,
  key-based SSH (`BatchMode`) from the Plakar host.
- **DRBD**: target the **Primary** node (block devices are only readable there).
- **Guest VM**: nothing.

No `qemu-img` and no `qemu-guest-agent` are required.

## Configuration

Only two keys (see [importer/schema.json](importer/schema.json)):

- `location` (required): `kvm://[<user>@]<host>/(system|session)`
  - `kvm:///system` → `qemu:///system` (local)
  - `kvm://FR-KVM-TEST1/system` → `qemu+ssh://FR-KVM-TEST1/system` (remote)
  - `kvm://root@FR-KVM-TEST1/system` → remote with SSH user `root`
- `domains` (optional): comma-separated VM names to include. Empty = all domains.

## Snapshot layout

Each domain produces the following tree inside the Kloset snapshot:

```
/<domain>/domain.xml
/<domain>/disks/<image-basename>
```

## Examples

```bash
# LOCAL: back up all VMs on the local hypervisor
$ plakar source add myKVM kvm:///system
$ plakar backup @myKVM

# REMOTE: back up VMs on a remote hypervisor over SSH (Plakar not installed there)
# requires key-based SSH from this host to root@FR-KVM-TEST1
$ plakar source add remoteKVM kvm://root@FR-KVM-TEST1/system
$ plakar backup @remoteKVM

# only specific VMs
$ plakar source add someVMs kvm://root@FR-KVM-TEST1/system domains=web01,web02
$ plakar backup @someVMs
```

## Restore

The `kvm` **exporter** restores a snapshot (domain XML + disk images) as files on
the target hypervisor, under `/var/lib/libvirt/images/plakar-restore/<domain>/`
(local or over SSH). It intentionally does **not** redefine or start the domain
and never writes to DRBD/production devices — restoring is a deliberate,
reviewable operation.

```bash
# restore a snapshot onto the hypervisor (files land under plakar-restore/)
$ plakar restore -to kvm://root@FR-KVM-TEST1/system <snapid>
```

Result on the hypervisor:

```
/var/lib/libvirt/images/plakar-restore/<domain>/domain.xml
/var/lib/libvirt/images/plakar-restore/<domain>/disks/<image-basename>
```

Then, to bring the VM back (manual, so you control name/placement):

```bash
# [hypervisor] adjust the definition and put the disk where it belongs
$ cd /var/lib/libvirt/images/plakar-restore/<domain>
# option A — run from the restored file: edit domain.xml <disk> source to point
#   at disks/<image-basename>, then:
$ virsh define domain.xml && virsh start <domain>
# option B — restore onto a (new/dedicated) DRBD/block device:
$ qemu-img convert -O raw disks/<image-basename> /dev/<target-volume>
#   then define with the original XML.
```

> The restored disk is the raw content captured at snapshot time. For a
> DRBD-backed VM, write it to a **dedicated/new** volume for testing — never
> overwrite the live Primary device without a maintenance window.

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
- Crash-consistent disk backup via atomic external snapshot (running domains) or
  direct read (stopped domains).
- DRBD block devices as sources on the Primary node.
- Local and remote (SSH) modes — Plakar need not be installed on the hypervisor.

Planned:
- Application-consistent snapshots via qemu-guest-agent quiesce (once a reliable
  guest-agent setup is validated).
- Incremental backups via QEMU dirty bitmaps.
- An **exporter** (restore) counterpart to redefine domains and restore disks.

## License

ISC — see [LICENSE](LICENSE).
