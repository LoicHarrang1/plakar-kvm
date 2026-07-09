package importer

import "testing"

func TestResolveConnectURI(t *testing.T) {
	tests := []struct {
		name    string
		loc     string
		want    string
		wantErr bool
	}{
		{"local system", "kvm:///system", "qemu:///system", false},
		{"local session", "kvm:///session", "qemu:///session", false},
		{"remote host", "kvm://node1/system", "qemu+ssh://node1/system", false},
		{"remote with user", "kvm://root@node1/system", "qemu+ssh://root@node1/system", false},
		{"missing location", "", "", true},
		{"wrong scheme", "qemu:///system", "", true},
		{"bad path", "kvm:///bogus", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveConnectURI(tt.loc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveConnectURI() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("resolveConnectURI() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseDomblklist(t *testing.T) {
	out := []byte(`
 Type    Device   Target   Source
------------------------------------------------------
 file    disk     vda      /var/lib/libvirt/images/vm.qcow2
 block   disk     vdb      /dev/drbd0
 file    cdrom    sda      /var/lib/libvirt/images/seed.iso
 file    disk     vdc      -
`)

	disks := parseDomblklist(out)
	if len(disks) != 2 {
		t.Fatalf("expected 2 disks (cdrom and '-' source skipped), got %d: %+v", len(disks), disks)
	}
	if disks[0].Target != "vda" || disks[0].Source != "/var/lib/libvirt/images/vm.qcow2" {
		t.Errorf("disk[0] = %+v", disks[0])
	}
	if disks[1].DevType != "block" || disks[1].Source != "/dev/drbd0" {
		t.Errorf("disk[1] (DRBD) = %+v", disks[1])
	}
}

func TestParseDomainList(t *testing.T) {
	out := []byte("web01\n\ndb01\n  \nmail01\n")
	got := parseDomainList(out)
	want := []string{"web01", "db01", "mail01"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestNewAccess(t *testing.T) {
	tests := []struct {
		name       string
		connectURI string
		wantRemote bool
		wantTarget string // only checked when remote
		wantPort   string
	}{
		{"local system", "qemu:///system", false, "", ""},
		{"local session", "qemu:///session", false, "", ""},
		{"remote ssh", "qemu+ssh://root@node1/system", true, "root@node1", ""},
		{"remote no user", "qemu+ssh://node1/system", true, "node1", ""},
		{"remote with port", "qemu+ssh://root@node1:2200/system", true, "root@node1", "2200"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc, err := newAccess(tt.connectURI)
			if err != nil {
				t.Fatalf("newAccess() error = %v", err)
			}
			if !tt.wantRemote {
				if _, ok := acc.(localAccess); !ok {
					t.Fatalf("expected localAccess, got %T", acc)
				}
				return
			}
			ssh, ok := acc.(sshAccess)
			if !ok {
				t.Fatalf("expected sshAccess, got %T", acc)
			}
			if ssh.target != tt.wantTarget {
				t.Errorf("target = %q, want %q", ssh.target, tt.wantTarget)
			}
			if ssh.port != tt.wantPort {
				t.Errorf("port = %q, want %q", ssh.port, tt.wantPort)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/dev/drbd0":              `'/dev/drbd0'`,
		"/var/lib/foo bar.qcow2":  `'/var/lib/foo bar.qcow2'`,
		"weird'name":              `'weird'\''name'`,
		"/path/$(rm -rf x).qcow2": `'/path/$(rm -rf x).qcow2'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNvramPath(t *testing.T) {
	uefi := []byte(`<domain type='kvm'>
  <name>win11</name>
  <os firmware='efi'>
    <type arch='x86_64' machine='q35'>hvm</type>
    <loader readonly='yes' type='pflash'>/usr/share/OVMF/OVMF_CODE.fd</loader>
    <nvram template='/usr/share/OVMF/OVMF_VARS.fd'>/var/lib/libvirt/qemu/nvram/win11_VARS.fd</nvram>
  </os>
</domain>`)
	if got := nvramPath(uefi); got != "/var/lib/libvirt/qemu/nvram/win11_VARS.fd" {
		t.Errorf("nvramPath(uefi) = %q, want the vars file path", got)
	}

	bios := []byte(`<domain type='kvm'><name>lin</name><os><type>hvm</type><boot dev='hd'/></os></domain>`)
	if got := nvramPath(bios); got != "" {
		t.Errorf("nvramPath(bios) = %q, want empty (no NVRAM)", got)
	}

	if got := nvramPath([]byte("not xml")); got != "" {
		t.Errorf("nvramPath(garbage) = %q, want empty", got)
	}
}

func TestVMPath(t *testing.T) {
	cases := map[string]string{
		"":     "/",
		"a":    "/a",
		"a/b":  "/a/b",
		"a//b": "/a/b",
	}
	// vmPath must always return an absolute path rooted at "/".
	if got := vmPath("web01", "domain.xml"); got != "/web01/domain.xml" {
		t.Errorf("vmPath(web01, domain.xml) = %q, want /web01/domain.xml", got)
	}
	if got := vmPath("web01", "disks", "vda"); got != "/web01/disks/vda" {
		t.Errorf("vmPath(web01, disks, vda) = %q, want /web01/disks/vda", got)
	}
	for in, want := range cases {
		if got := vmPath(in); got != want {
			t.Errorf("vmPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildSnapshotArgs(t *testing.T) {
	disks := []diskInfo{
		{DevType: "file", Device: "disk", Target: "vda", Source: "/var/lib/libvirt/images/vm.qcow2"},
		{DevType: "block", Device: "disk", Target: "vdb", Source: "/dev/drbd0"},
	}
	args, overlays, targets := buildSnapshotArgs("web01", "plakar-1700000000", disks, "/var/lib/libvirt/images", 1700000000)

	// header
	want := []string{"snapshot-create-as", "web01", "plakar-1700000000", "--disk-only", "--atomic", "--no-metadata"}
	for i, w := range want {
		if args[i] != w {
			t.Fatalf("args[%d] = %q, want %q", i, args[i], w)
		}
	}

	// one --diskspec per disk, each with snapshot=external and an overlay file
	if got := countOccurrences(args, "--diskspec"); got != 2 {
		t.Fatalf("expected 2 --diskspec, got %d in %v", got, args)
	}
	wantSpec := "vda,snapshot=external,file=/var/lib/libvirt/images/web01-vda-plakar-1700000000.qcow2"
	if !contains(args, wantSpec) {
		t.Errorf("missing diskspec %q in %v", wantSpec, args)
	}

	if len(overlays) != 2 || overlays[1] != "/var/lib/libvirt/images/web01-vdb-plakar-1700000000.qcow2" {
		t.Errorf("overlays = %v", overlays)
	}
	if len(targets) != 2 || targets[0] != "vda" || targets[1] != "vdb" {
		t.Errorf("targets = %v", targets)
	}
}

func countOccurrences(ss []string, v string) int {
	n := 0
	for _, s := range ss {
		if s == v {
			n++
		}
	}
	return n
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
