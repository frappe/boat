package backup

import (
	"context"
	"testing"
)

const restoreWork = "/var/lib/atlas/tmp/s3-restore-snap-golden"

func withHealthyPool(fake *fakeCommands) *fakeCommands {
	return fake.
		output("sudo lvs --noheadings -o data_percent atlas/pool0", " 42.00").
		output("sudo lvs --noheadings -o metadata_percent atlas/pool0", " 7.00")
}

// Rehydrate a disk snapshot: download the compressed image, VERIFY its sha256, and
// only then recreate a clean thin LV and decompress onto it. Here the LV does not
// exist yet, so the guarded remove is a no-op and CreateThin does the lvcreate.
func TestRestoreSnapshotS3BlockObject(t *testing.T) {
	rootfsTemp := restoreWork + "/rootfs.zst"
	fake := withHealthyPool(newFakeCommands()).
		exists("test -b " + snapDevice)

	objects := []BackupObject{{
		Name: "rootfs", ObjectName: "rootfs.zst", Source: snapDevice,
		Block: true, Compress: true, DiskGigabytes: 28, URL: "https://get-rootfs", SHA256: "aaa111",
	}}
	result, err := RestoreSnapshotS3(context.Background(), fake, RestoreSnapshotParams{
		SnapshotName: "snap-golden", Objects: objects,
	})
	if err != nil {
		t.Fatalf("RestoreSnapshotS3: %v", err)
	}
	if len(result.Objects) != 1 || result.Objects[0] != "rootfs" {
		t.Errorf("result = %+v", result)
	}

	assertTrace(t, fake,
		"sudo lvs --noheadings -o data_percent atlas/pool0",
		"sudo lvs --noheadings -o metadata_percent atlas/pool0",
		"sudo rm -rf "+restoreWork,
		"install-dir 0700 "+restoreWork,
		"sudo curl --fail --silent --show-error --output "+rootfsTemp+" https://get-rootfs",
		"aaa111  "+rootfsTemp+" | sudo sha256sum -c -",
		"? sudo lvs --noheadings atlas/"+snapLV, // Remove's presence probe (absent → no-op)
		"? sudo lvs --noheadings atlas/"+snapLV, // CreateThin's presence probe (absent → create)
		"sudo lvcreate --type thin --thinpool atlas/pool0 -V 28G -n "+snapLV+" atlas",
		"sudo lvchange -ay -K atlas/"+snapLV,
		"sudo udevadm settle",
		"? test -b "+snapDevice,
		"sudo zstd -d -q -f --sparse -o "+snapDevice+" "+rootfsTemp,
		"sudo rm -f "+rootfsTemp,
		"sudo rm -rf "+restoreWork,
		"sudo sync",
	)

	// The integrity gate: the sha256 verify must come BEFORE the decompress, so
	// corrupt bytes never reach a decompressor pointed at a live LV.
	if indexOf(t, fake, "sha256sum -c -") >= indexOf(t, fake, "zstd -d") {
		t.Error("sha256 was verified AFTER decompress, defeating the integrity gate")
	}
}

// When the LV already exists, restore removes it before recreating clean — a
// re-restore must not decompress onto stale blocks.
func TestRestoreSnapshotS3RemovesExistingLV(t *testing.T) {
	fake := withHealthyPool(newFakeCommands()).
		exists("sudo lvs --noheadings atlas/" + snapLV).
		exists("test -b " + snapDevice)

	_, err := RestoreSnapshotS3(context.Background(), fake, RestoreSnapshotParams{
		SnapshotName: "snap-golden",
		Objects: []BackupObject{{
			Name: "rootfs", ObjectName: "rootfs.zst", Source: snapDevice,
			Block: true, Compress: true, DiskGigabytes: 28, URL: "https://get", SHA256: "aaa111",
		}},
	})
	if err != nil {
		t.Fatalf("RestoreSnapshotS3: %v", err)
	}
	assertIssued(t, fake, "sudo lvremove -f atlas/"+snapLV)
}

// A warm memory file: verify, then decompress into the recreated memory directory
// with root:root 0644 — matching the warm capture's durable staging.
func TestRestoreSnapshotS3FileObject(t *testing.T) {
	memory := "/var/lib/atlas/snapshots/snap-golden/mem.bin"
	memoryDir := "/var/lib/atlas/snapshots/snap-golden"
	memoryTemp := restoreWork + "/mem.zst"
	fake := newFakeCommands()

	_, err := RestoreSnapshotS3(context.Background(), fake, RestoreSnapshotParams{
		SnapshotName: "snap-golden",
		Objects: []BackupObject{{
			Name: "mem", ObjectName: "mem.zst", Source: memory,
			Block: false, Compress: true, URL: "https://get-mem", SHA256: "ddd444",
		}},
	})
	if err != nil {
		t.Fatalf("RestoreSnapshotS3: %v", err)
	}
	// No pool read for a memory-only rehydrate.
	assertNotIssued(t, fake, "data_percent")
	assertTrace(t, fake,
		"sudo rm -rf "+restoreWork,
		"install-dir 0700 "+restoreWork,
		"sudo curl --fail --silent --show-error --output "+memoryTemp+" https://get-mem",
		"ddd444  "+memoryTemp+" | sudo sha256sum -c -",
		"install-dir 0755 "+memoryDir,
		"sudo zstd -d -q -f -o "+memory+" "+memoryTemp,
		"sudo chown root:root "+memory,
		"sudo chmod 0644 "+memory,
		"sudo rm -f "+memoryTemp,
		"sudo rm -rf "+restoreWork,
		"sudo sync",
	)
}

// A pool too full to take a recreated thin LV refuses before any download.
func TestRestoreSnapshotS3RefusesFullPool(t *testing.T) {
	fake := newFakeCommands().
		output("sudo lvs --noheadings -o data_percent atlas/pool0", "95.00").
		output("sudo lvs --noheadings -o metadata_percent atlas/pool0", "5.00")

	_, err := RestoreSnapshotS3(context.Background(), fake, RestoreSnapshotParams{
		SnapshotName: "snap-golden",
		Objects: []BackupObject{{
			Name: "rootfs", ObjectName: "rootfs.zst", Source: snapDevice,
			Block: true, Compress: true, DiskGigabytes: 28, URL: "https://get", SHA256: "aaa111",
		}},
	})
	if err == nil {
		t.Fatal("RestoreSnapshotS3 accepted a full pool")
	}
	assertNotIssued(t, fake, "curl")
	assertNotIssued(t, fake, "lvcreate")
}

// A block object with no disk size cannot recreate its LV — fail loud rather than
// create a zero-length disk.
func TestRestoreSnapshotS3RejectsBlockWithoutDiskSize(t *testing.T) {
	fake := withHealthyPool(newFakeCommands())

	_, err := RestoreSnapshotS3(context.Background(), fake, RestoreSnapshotParams{
		SnapshotName: "snap-golden",
		Objects: []BackupObject{{
			Name: "rootfs", ObjectName: "rootfs.zst", Source: snapDevice,
			Block: true, Compress: true, URL: "https://get", SHA256: "aaa111", // DiskGigabytes 0
		}},
	})
	if err == nil {
		t.Fatal("RestoreSnapshotS3 accepted a block object with no disk size")
	}
	assertNotIssued(t, fake, "lvcreate")
}
