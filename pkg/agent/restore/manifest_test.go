package restore

import (
	"strings"
	"testing"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	backupmodel "github.com/cloud-barista/ai-ha/pkg/backup/model"
)

func TestCheckManifestRejects(t *testing.T) {
	plan := model.RestorePlan{SourceSystemID: testSystemID, SourceBackupSetID: testSetID}
	tests := []struct {
		name   string
		mutate func(*backupmodel.Manifest)
		want   string
	}{
		{"newer manifest version", func(m *backupmodel.Manifest) { m.Version++ }, "version"},
		{"another system's set", func(m *backupmodel.Manifest) { m.Set.SystemID = "other" }, "belongs to system"},
		{"failed set", func(m *backupmodel.Manifest) { m.Set.Status = backupmodel.BackupFailed }, "did not complete"},
		{"file name escaping the system directory", func(m *backupmodel.Manifest) { m.Set.Items[0].FileName = "../x.aha" }, "unusable file name"},
		{"unknown codec", func(m *backupmodel.Manifest) { m.Set.Items[0].Compression = "lz4" }, "unsupported compression"},
		{"no system facts", func(m *backupmodel.Manifest) { m.System = nil }, "no system information"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testManifest(t)
			if err := checkManifest(m, plan); err != nil {
				t.Fatalf("valid manifest rejected: %v", err)
			}
			tt.mutate(&m)
			if err := checkManifest(m, plan); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tt.want)
			}
		})
	}
}

func TestPlanTargetsOrdersByDepthAndReadsLabels(t *testing.T) {
	m := testManifest(t)
	// Listed deepest first, as nothing in a manifest promises an order.
	m.Set.Items[0], m.Set.Items[2] = m.Set.Items[2], m.Set.Items[0]
	info, err := decodeSystem(m.System)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := planTargets(m.Set, info, "/mnt")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// The target names its disk differently from the source; PARTUUIDs match.
	listing := "/dev/nvme0n1\n"
	for _, p := range testPartitions {
		listing += p.device + " " + strings.ToUpper(p.partUUID) + "\n"
	}
	if err := resolveDevices(targets, listing, "/mnt"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var got []string
	for _, tg := range targets {
		got = append(got, tg.item.MountPoint+"="+tg.mountDir+","+tg.fsType+","+tg.label)
	}
	want := "/=/mnt/nvme0n1p1,ext4,cloudimg-rootfs /boot=/mnt/nvme0n1p16,ext4,BOOT /boot/efi=/mnt/nvme0n1p15,vfat,UEFI"
	if strings.Join(got, " ") != want {
		t.Fatalf("targets = %v\nwant %s", got, want)
	}
}
