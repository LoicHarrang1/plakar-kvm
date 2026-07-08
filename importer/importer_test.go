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

func TestParseConsistency(t *testing.T) {
	if m, _ := parseConsistency(""); m != modeFsfreeze {
		t.Errorf("default consistency = %q, want fsfreeze", m)
	}
	if _, err := parseConsistency("bogus"); err == nil {
		t.Errorf("expected error for invalid consistency mode")
	}
}
