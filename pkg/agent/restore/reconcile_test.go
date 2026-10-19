package restore

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const (
	oldRootUUID = "11111111-2222-3333-4444-555555555555"
	newRootUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	oldBootUUID = "66666666-7777-8888-9999-000000000000"
	newBootUUID = "ffffffff-0000-1111-2222-333333333333"
)

func TestRewriteFstab(t *testing.T) {
	in := "# /etc/fstab: static file system information.\n" +
		"UUID=" + oldRootUUID + "\t/\text4\tdiscard,errors=remount-ro\t0 1\n" +
		"UUID=" + oldBootUUID + " /boot ext4 defaults 0 2\n" +
		"LABEL=UEFI\t/boot/efi\tvfat\tumask=0077\t0 1\n" +
		"UUID=99999999-9999-9999-9999-999999999999 /data xfs defaults 0 2\n" +
		"#UUID=" + oldRootUUID + " / ext4 defaults 0 1\n"
	want := "# /etc/fstab: static file system information.\n" +
		"UUID=" + newRootUUID + "\t/\text4\tdiscard,errors=remount-ro\t0 1\n" +
		"UUID=" + newBootUUID + " /boot ext4 defaults 0 2\n" +
		"LABEL=UEFI\t/boot/efi\tvfat\tumask=0077\t0 1\n" +
		"UUID=99999999-9999-9999-9999-999999999999 /data xfs defaults 0 2\n" +
		"#UUID=" + oldRootUUID + " / ext4 defaults 0 1\n"

	got, changed := rewriteFstab(in, map[string]string{"/": newRootUUID, "/boot": newBootUUID, "/boot/efi": "ABCD-1234"})
	if !changed || got != want {
		t.Fatalf("changed=%v\ngot:\n%s\nwant:\n%s", changed, got, want)
	}
	if _, changed := rewriteFstab("LABEL=cloudimg-rootfs / ext4 defaults 0 1\n", map[string]string{"/": newRootUUID}); changed {
		t.Fatal("a LABEL= entry was rewritten")
	}
}

func TestNetplanMACsAndRewrite(t *testing.T) {
	files := map[string]string{
		"50-cloud-init.yaml": "network:\n  ethernets:\n    ens5:\n      match:\n        macaddress: 0A:1B:2C:3D:4E:5F\n      set-name: ens5\n",
		"60-extra.yaml":      "network:\n  ethernets:\n    ens5:\n      match: {macaddress: \"0a:1b:2c:3d:4e:5f\"}\n",
	}
	if got := netplanMACs(files); !reflect.DeepEqual(got, []string{"0a:1b:2c:3d:4e:5f"}) {
		t.Fatalf("netplanMACs = %v", got)
	}

	got, changed := rewriteNetplan(files["50-cloud-init.yaml"], "0a:1b:2c:3d:4e:5f", "02:aa:bb:cc:dd:ee")
	want := "network:\n  ethernets:\n    ens5:\n      match:\n        macaddress: 02:aa:bb:cc:dd:ee\n      set-name: ens5\n"
	if !changed || got != want {
		t.Fatalf("changed=%v got:\n%s", changed, got)
	}
}

func TestCleanGrubDefaults(t *testing.T) {
	tests := []struct {
		name, in, want string
		removed        []string
	}{
		{
			name:    "forced partuuid file of an Ubuntu cloud image",
			in:      "# Force the kernel to boot without an initrd\nGRUB_FORCE_PARTUUID=" + oldRootUUID + "\n",
			want:    "# Force the kernel to boot without an initrd\n",
			removed: []string{"GRUB_FORCE_PARTUUID"},
		},
		{
			name:    "MBR partuuid",
			in:      "GRUB_FORCE_PARTUUID=1a2b3c4d-01\nGRUB_TIMEOUT=0\n",
			want:    "GRUB_TIMEOUT=0\n",
			removed: []string{"GRUB_FORCE_PARTUUID"},
		},
		{
			name: "pinned kernel arguments",
			in: `GRUB_CMDLINE_LINUX_DEFAULT="console=tty1 root=UUID=` + oldRootUUID + ` resume=/dev/disk/by-uuid/` + oldBootUUID + ` nvme_core.io_timeout=4294967295"` + "\n" +
				`GRUB_CMDLINE_LINUX='root=PARTUUID=1a2b3c4d-01'` + "\n",
			want: `GRUB_CMDLINE_LINUX_DEFAULT="console=tty1 nvme_core.io_timeout=4294967295"` + "\n" +
				`GRUB_CMDLINE_LINUX=''` + "\n",
			removed: []string{
				"GRUB_CMDLINE_LINUX_DEFAULT: root=UUID=" + oldRootUUID,
				"GRUB_CMDLINE_LINUX_DEFAULT: resume=/dev/disk/by-uuid/" + oldBootUUID,
				"GRUB_CMDLINE_LINUX: root=PARTUUID=1a2b3c4d-01",
			},
		},
		{
			name: "nothing pinned",
			in:   "GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX_DEFAULT=\"quiet splash\"\nexport GRUB_TERMINAL=console\n",
			want: "GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX_DEFAULT=\"quiet splash\"\nexport GRUB_TERMINAL=console\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, removed := cleanGrubDefaults(tt.in)
			if got != tt.want {
				t.Fatalf("got:\n%s\nwant:\n%s", got, tt.want)
			}
			if !reflect.DeepEqual(removed, tt.removed) {
				t.Fatalf("removed = %q, want %q", removed, tt.removed)
			}
		})
	}
}

func TestRewriteEFIStub(t *testing.T) {
	stub := "search.fs_uuid " + oldBootUUID + " root\nset prefix=($root)'/grub'\nconfigfile $prefix/grub.cfg\n"
	got, changed := rewriteEFIStub(stub, newBootUUID)
	want := "search.fs_uuid " + newBootUUID + " root\nset prefix=($root)'/grub'\nconfigfile $prefix/grub.cfg\n"
	if !changed || got != want {
		t.Fatalf("changed=%v got:\n%s", changed, got)
	}

	long := "search --no-floppy --fs-uuid --set=dev " + oldBootUUID + "\nset prefix=($dev)/grub\nconfigfile $prefix/grub.cfg\n"
	got, changed = rewriteEFIStub(long, newBootUUID)
	if !changed || got != "search --no-floppy --fs-uuid --set=dev "+newBootUUID+"\nset prefix=($dev)/grub\nconfigfile $prefix/grub.cfg\n" {
		t.Fatalf("long form: changed=%v got:\n%s", changed, got)
	}

	full := "menuentry 'Ubuntu' {\n\tsearch --no-floppy --fs-uuid --set=root " + oldBootUUID + "\n}\nconfigfile x\n"
	if _, changed := rewriteEFIStub(full, newBootUUID); changed {
		t.Fatal("a full grub.cfg was treated as a stub")
	}
	if _, changed := rewriteEFIStub("search.fs_label BOOT root\nconfigfile $prefix/grub.cfg\n", newBootUUID); changed {
		t.Fatal("a label-based stub was changed")
	}
}

func TestRewriteFileKeepsModeAndSkipsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fstab")
	if err := os.WriteFile(path, []byte("a\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	changed, err := rewriteFile(path, func(c string) (string, bool) { return c, false })
	if err != nil || changed {
		t.Fatalf("unchanged edit: changed=%v err=%v", changed, err)
	}
	changed, err = rewriteFile(path, func(string) (string, bool) { return "b\n", true })
	if err != nil || !changed {
		t.Fatalf("edit: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if string(data) != "b\n" || info.Mode().Perm() != 0o640 {
		t.Fatalf("file = %q mode %o", data, info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary file left behind: %v", entries)
	}
}

func TestGrubPinnedIDs(t *testing.T) {
	in := "GRUB_FORCE_PARTUUID=" + oldRootUUID + "\n" +
		`GRUB_CMDLINE_LINUX_DEFAULT="console=tty1 root=UUID=` + oldBootUUID + ` resume=/dev/disk/by-partuuid/1a2b3c4d-02 quiet"` + "\n" +
		"GRUB_TIMEOUT=0\n# GRUB_FORCE_PARTUUID is commented out below\n#GRUB_FORCE_PARTUUID=" + newRootUUID + "\n"
	got := grubPinnedIDs(in)
	want := []pinnedID{
		{where: "GRUB_FORCE_PARTUUID", value: oldRootUUID},
		{where: "GRUB_CMDLINE_LINUX_DEFAULT root=UUID=" + oldBootUUID, value: oldBootUUID},
		{where: "GRUB_CMDLINE_LINUX_DEFAULT resume=/dev/disk/by-partuuid/1a2b3c4d-02", value: "1a2b3c4d-02"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("grubPinnedIDs =\n%+v\nwant\n%+v", got, want)
	}
}

func TestMkfsArgsPreserveUUID(t *testing.T) {
	tests := []struct {
		fsType, label, uuid string
		want                []string
	}{
		{"ext4", "cloudimg-rootfs", oldRootUUID, []string{"-F", "-q", "-U", oldRootUUID, "-L", "cloudimg-rootfs", "/dev/x"}},
		{"xfs", "", oldRootUUID, []string{"-f", "-q", "-m", "uuid=" + oldRootUUID, "/dev/x"}},
		{"vfat", "UEFI", "ABCD-1234", []string{"-i", "ABCD1234", "-n", "UEFI", "/dev/x"}},
	}
	for _, tt := range tests {
		if got := mkfsArgs(tt.fsType, tt.label, tt.uuid, "/dev/x"); !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("%s: %q, want %q", tt.fsType, got, tt.want)
		}
	}
}
