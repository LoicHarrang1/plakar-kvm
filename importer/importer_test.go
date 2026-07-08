package importer

import "testing"

func TestResolveConnectURI(t *testing.T) {
	tests := []struct {
		name    string
		config  map[string]string
		want    string
		wantErr bool
	}{
		{"local system", map[string]string{"location": "kvm:///system"}, "qemu:///system", false},
		{"local session", map[string]string{"location": "kvm:///session"}, "qemu:///session", false},
		{"remote host", map[string]string{"location": "kvm://node1/system"}, "qemu+ssh://node1/system", false},
		{"explicit override", map[string]string{"location": "kvm:///system", "connect_uri": "qemu+ssh://root@n2/system"}, "qemu+ssh://root@n2/system", false},
		{"missing location", map[string]string{}, "", true},
		{"wrong scheme", map[string]string{"location": "qemu:///system"}, "", true},
		{"bad path", map[string]string{"location": "kvm:///bogus"}, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveConnectURI(tt.config)
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
		config     map[string]string
		wantRemote bool
		wantTarget string // only checked when remote
		wantPort   string
		wantErr    bool
	}{
		{"local system", "qemu:///system", nil, false, "", "", false},
		{"local session", "qemu:///session", nil, false, "", "", false},
		{"remote ssh", "qemu+ssh://root@node1/system", nil, true, "root@node1", "", false},
		{"remote no user", "qemu+ssh://node1/system", nil, true, "node1", "", false},
		{"ssh overrides", "qemu+ssh://root@node1/system", map[string]string{"ssh_user": "backup", "ssh_port": "2222"}, true, "backup@node1", "2222", false},
		{"remote with port in uri", "qemu+ssh://root@node1:2200/system", nil, true, "root@node1", "2200", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc, remote, err := newAccess(tt.connectURI, tt.config)
			if (err != nil) != tt.wantErr {
				t.Fatalf("newAccess() error = %v, wantErr %v", err, tt.wantErr)
			}
			if remote != tt.wantRemote {
				t.Fatalf("newAccess() remote = %v, want %v", remote, tt.wantRemote)
			}
			if !remote {
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

func TestParseConsistency(t *testing.T) {
	if m, _ := parseConsistency(""); m != modeFsfreeze {
		t.Errorf("default consistency = %q, want fsfreeze", m)
	}
	if _, err := parseConsistency("bogus"); err == nil {
		t.Errorf("expected error for invalid consistency mode")
	}
}
