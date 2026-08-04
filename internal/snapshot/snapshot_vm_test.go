package snapshot

import (
	"context"
	"testing"
)

const (
	vmDisk       = "atlas-vm-" + testUUID
	dataDisk     = "atlas-data-" + testUUID
	rootSnap     = "atlas-snap-" + testUUID
	dataSnap     = "atlas-datasnap-" + testUUID
	rootSnapPath = "/dev/atlas/" + rootSnap
	dataSnapPath = "/dev/atlas/" + dataSnap
	rootSnapDev  = "/dev/atlas/" + rootSnap
	dataSnapDev  = "/dev/atlas/" + dataSnap
)

// withHealthyPool scripts the two pool-fill reads well under the threshold.
func withHealthyPool(fake *fakeCommands) *fakeCommands {
	return fake.
		output("sudo lvs --noheadings -o data_percent atlas/pool0", " 42.00").
		output("sudo lvs --noheadings -o metadata_percent atlas/pool0", " 7.00")
}

// A Stopped VM with a root disk and no data disk: check the disk is here, the pool
// has room, thin-snapshot the disk, and report its size.
func TestSnapshotVMRootOnly(t *testing.T) {
	fake := withHealthyPool(newFakeCommands()).
		exists("sudo lvs --noheadings atlas/"+vmDisk).
		exists("test -b "+rootSnapDev).
		output("sudo blockdev --getsize64 "+rootSnapDev, "10737418240\n")

	result, err := SnapshotVM(context.Background(), fake, SnapshotVMParams{
		UUID: testUUID, SnapshotRootfsPath: rootSnapPath,
	})
	if err != nil {
		t.Fatalf("SnapshotVM: %v", err)
	}
	if result.SizeBytes != 10737418240 || result.DataSizeBytes != 0 {
		t.Errorf("result = %+v", result)
	}
	assertTrace(t, fake,
		"? sudo lvs --noheadings atlas/"+vmDisk,
		"sudo lvs --noheadings -o data_percent atlas/pool0",
		"sudo lvs --noheadings -o metadata_percent atlas/pool0",
		"? sudo lvs --noheadings atlas/"+rootSnap,
		"sudo lvcreate -s atlas/"+vmDisk+" -n "+rootSnap,
		"sudo lvchange -ay -K atlas/"+rootSnap,
		"sudo udevadm settle",
		"? test -b "+rootSnapDev,
		"sudo blockdev --getsize64 "+rootSnapDev,
	)
}

// A VM with a data disk: both disks are snapshotted, and both sizes come back.
func TestSnapshotVMWithDataDisk(t *testing.T) {
	fake := withHealthyPool(newFakeCommands()).
		exists("sudo lvs --noheadings atlas/"+vmDisk).
		exists("sudo lvs --noheadings atlas/"+dataDisk).
		exists("test -b "+rootSnapDev).
		exists("test -b "+dataSnapDev).
		output("sudo blockdev --getsize64 "+rootSnapDev, "10737418240\n").
		output("sudo blockdev --getsize64 "+dataSnapDev, "21474836480\n")

	result, err := SnapshotVM(context.Background(), fake, SnapshotVMParams{
		UUID: testUUID, SnapshotRootfsPath: rootSnapPath, DataSnapshotRootfsPath: dataSnapPath,
	})
	if err != nil {
		t.Fatalf("SnapshotVM: %v", err)
	}
	if result.SizeBytes != 10737418240 || result.DataSizeBytes != 21474836480 {
		t.Errorf("result = %+v", result)
	}
	assertIssued(t, fake, "sudo lvcreate -s atlas/"+dataDisk+" -n "+dataSnap)
	assertIssued(t, fake, "sudo blockdev --getsize64 "+dataSnapDev)
}

// A row that claims a data disk whose LV is gone: root is still captured, the data
// half is skipped without error.
func TestSnapshotVMToleratesMissingDataDisk(t *testing.T) {
	fake := withHealthyPool(newFakeCommands()).
		exists("sudo lvs --noheadings atlas/"+vmDisk).
		exists("test -b "+rootSnapDev).
		output("sudo blockdev --getsize64 "+rootSnapDev, "10737418240\n")
	// data disk LV NOT scripted present → absent.

	result, err := SnapshotVM(context.Background(), fake, SnapshotVMParams{
		UUID: testUUID, SnapshotRootfsPath: rootSnapPath, DataSnapshotRootfsPath: dataSnapPath,
	})
	if err != nil {
		t.Fatalf("SnapshotVM: %v", err)
	}
	if result.DataSizeBytes != 0 {
		t.Errorf("captured a size for an absent data disk: %+v", result)
	}
	assertNotIssued(t, fake, "-n "+dataSnap)
}

// A source disk that is not on this host fails loud before the pool is even read,
// and snapshots nothing.
func TestSnapshotVMRejectsMissingDisk(t *testing.T) {
	fake := withHealthyPool(newFakeCommands()) // vmDisk not present

	if _, err := SnapshotVM(context.Background(), fake, SnapshotVMParams{
		UUID: testUUID, SnapshotRootfsPath: rootSnapPath,
	}); err == nil {
		t.Fatal("SnapshotVM accepted a missing source disk")
	}
	assertNotIssued(t, fake, "lvcreate")
	assertNotIssued(t, fake, "data_percent")
}

// A pool at or past the fullness threshold refuses to snapshot.
func TestSnapshotVMRefusesFullPool(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo lvs --noheadings atlas/"+vmDisk).
		output("sudo lvs --noheadings -o data_percent atlas/pool0", "93.10").
		output("sudo lvs --noheadings -o metadata_percent atlas/pool0", "5.00")

	if _, err := SnapshotVM(context.Background(), fake, SnapshotVMParams{
		UUID: testUUID, SnapshotRootfsPath: rootSnapPath,
	}); err == nil {
		t.Fatal("SnapshotVM accepted a full pool")
	}
	assertNotIssued(t, fake, "lvcreate")
}
