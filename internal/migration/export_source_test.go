package migration

import (
	"context"
	"testing"
)

// The source-side LV, snapshot and device names for the shared test UUID, spelled
// out so the golden reads like a host's journal.
const (
	vmDisk     = "atlas-vm-3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	dataDisk   = "atlas-data-3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	rootSnap   = "atlas-snap-3f2504e0-4f89-41d3-9a0c-0305e82c3301-migrate"
	dataSnap   = "atlas-datasnap-3f2504e0-4f89-41d3-9a0c-0305e82c3301-migrate"
	rootSnapDe = "/dev/atlas/atlas-snap-3f2504e0-4f89-41d3-9a0c-0305e82c3301-migrate"
	dataSnapDe = "/dev/atlas/atlas-datasnap-3f2504e0-4f89-41d3-9a0c-0305e82c3301-migrate"
)

// withHealthyPool scripts the two pool-fill reads as well under both thresholds.
func withHealthyPool(fake *fakeCommands) *fakeCommands {
	return fake.
		output("sudo lvs --noheadings -o data_percent atlas/pool0", " 42.00").
		output("sudo lvs --noheadings -o metadata_percent atlas/pool0", " 7.00")
}

func TestExportSourceRootOnly(t *testing.T) {
	fake := withHealthyPool(newFakeCommands()).
		exists("sudo lvs --noheadings atlas/"+vmDisk).
		exists("test -b "+rootSnapDe).
		exists("sudo test -f /var/lib/atlas/run/migrate-nbd-11165.pid").
		output("sudo cat /var/lib/atlas/run/migrate-nbd-11165.pid", "4242\n").
		output("sudo blockdev --getsize64 "+rootSnapDe, "10737418240\n")

	result, err := ExportSource(context.Background(), fake, testUUID, ExportSourceParams{BindAddress: testBindIPv4})
	if err != nil {
		t.Fatalf("ExportSource: %v", err)
	}
	want := ExportSourceResult{NBDPort: 11165, NBDPID: 4242, RootSizeBytes: 10737418240}
	if result != want {
		t.Errorf("result = %+v, want %+v", result, want)
	}

	assertTrace(t, fake,
		"sudo lvs --noheadings -o data_percent atlas/pool0",
		"sudo lvs --noheadings -o metadata_percent atlas/pool0",
		"sudo mkdir -p /var/lib/atlas/run",
		"? sudo lvs --noheadings atlas/"+vmDisk,
		"? sudo lvs --noheadings atlas/"+rootSnap,
		"sudo lvcreate -s atlas/"+vmDisk+" -n "+rootSnap,
		"sudo lvchange -ay -K atlas/"+rootSnap,
		"sudo udevadm settle",
		"? test -b "+rootSnapDe,
		"? sudo lvs --noheadings atlas/"+dataDisk,
		"- ss -ltn",
		"sudo qemu-nbd --persistent --read-only --cache=none --bind=203.0.113.7 --port=11165 --pid-file=/var/lib/atlas/run/migrate-nbd-11165.pid --fork "+rootSnapDe,
		"? sudo test -f /var/lib/atlas/run/migrate-nbd-11165.pid",
		"sudo cat /var/lib/atlas/run/migrate-nbd-11165.pid",
		"sudo blockdev --getsize64 "+rootSnapDe,
	)
}

func TestExportSourceWithDataDisk(t *testing.T) {
	fake := withHealthyPool(newFakeCommands()).
		exists("sudo lvs --noheadings atlas/"+vmDisk).
		exists("sudo lvs --noheadings atlas/"+dataDisk).
		exists("test -b "+rootSnapDe).
		exists("test -b "+dataSnapDe).
		exists("sudo test -f /var/lib/atlas/run/migrate-nbd-11165.pid").
		output("sudo cat /var/lib/atlas/run/migrate-nbd-11165.pid", "4242\n").
		output("sudo blockdev --getsize64 "+rootSnapDe, "10737418240\n").
		output("sudo blockdev --getsize64 "+dataSnapDe, "21474836480\n")

	result, err := ExportSource(context.Background(), fake, testUUID, ExportSourceParams{BindAddress: testBindIPv4})
	if err != nil {
		t.Fatalf("ExportSource: %v", err)
	}
	want := ExportSourceResult{NBDPort: 11165, NBDPID: 4242, RootSizeBytes: 10737418240, DataSizeBytes: 21474836480}
	if result != want {
		t.Errorf("result = %+v, want %+v", result, want)
	}
	// The data disk is snapshotted, and its export lands on port+1 = 11166.
	assertIssued(t, fake, "sudo lvcreate -s atlas/"+dataDisk+" -n "+dataSnap)
	assertIssued(t, fake, "--port=11166 --pid-file=/var/lib/atlas/run/migrate-nbd-11166.pid --fork "+dataSnapDe)
}

// A pool at or past the fullness threshold refuses to snapshot before it touches a
// thing — the guard is the first pair of reads and then a hard stop.
func TestExportSourceRefusesFullPool(t *testing.T) {
	fake := newFakeCommands().
		output("sudo lvs --noheadings -o data_percent atlas/pool0", "93.10").
		output("sudo lvs --noheadings -o metadata_percent atlas/pool0", "5.00")

	if _, err := ExportSource(context.Background(), fake, testUUID, ExportSourceParams{BindAddress: testBindIPv4}); err == nil {
		t.Fatal("ExportSource accepted a full pool")
	}
	assertNotIssued(t, fake, "lvcreate")
	assertNotIssued(t, fake, "qemu-nbd")
}

// A source that is not on this host fails loud after the pool check and the mkdir,
// and snapshots nothing.
func TestExportSourceRejectsMissingDisk(t *testing.T) {
	fake := withHealthyPool(newFakeCommands()) // vmDisk not scripted present

	if _, err := ExportSource(context.Background(), fake, testUUID, ExportSourceParams{BindAddress: testBindIPv4}); err == nil {
		t.Fatal("ExportSource accepted a missing source disk")
	}
	assertNotIssued(t, fake, "lvcreate")
	assertNotIssued(t, fake, "qemu-nbd")
}
