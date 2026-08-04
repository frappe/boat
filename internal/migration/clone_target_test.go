package migration

import (
	"context"
	"testing"
)

const (
	imageDir      = "/var/lib/atlas/images/debian12"
	baseImage     = "atlas-image-debian12"
	vmDiskDev     = "/dev/atlas/atlas-vm-3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	vmCloneRoot   = "atlas-vm-3f2504e0-4f89-41d3-9a0c-0305e82c3301-clone"
	cloneMetaRoot = "atlas-clonemeta-3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	cloneMetaDev  = "/dev/atlas/atlas-clonemeta-3f2504e0-4f89-41d3-9a0c-0305e82c3301"
)

// withCloneDeps scripts the target migration deps present and the pool healthy —
// the common floor every prepare stands on.
func withCloneDeps(fake *fakeCommands) *fakeCommands {
	return withHealthyPool(fake).
		exists("sudo modprobe nbd").
		exists("sudo modprobe dm_clone").
		exists("which nbd-client").
		exists("sudo lvs --noheadings atlas/" + baseImage).
		exists("test -d " + imageDir)
}

func TestCloneTargetFreshPrepare(t *testing.T) {
	fake := withCloneDeps(newFakeCommands()).
		exists("test -b "+vmDiskDev).
		exists("test -b "+cloneMetaDev).
		output("sudo blockdev --getsize64 "+vmDiskDev, "10737418240\n")

	params := CloneTargetParams{ImageName: "debian12", DiskGB: 10, SourceHost: testSource}
	result, err := CloneTarget(context.Background(), fake, testUUID, params)
	if err != nil {
		t.Fatalf("CloneTarget: %v", err)
	}
	if result.RootCloneDevice != "/dev/mapper/"+vmCloneRoot || result.DataCloneDevice != "" {
		t.Errorf("result = %+v", result)
	}

	assertTrace(t, fake,
		"? sudo modprobe nbd",
		"? sudo modprobe dm_clone",
		"? which nbd-client",
		"? sudo lvs --noheadings atlas/"+baseImage,
		"? test -d "+imageDir,
		"sudo lvs --noheadings -o data_percent atlas/pool0",
		"sudo lvs --noheadings -o metadata_percent atlas/pool0",
		// create the dest thin LV
		"? sudo lvs --noheadings atlas/"+vmDisk,
		"sudo lvcreate --type thin --thinpool atlas/pool0 -V 10G -n "+vmDisk+" atlas",
		"sudo lvchange -ay -K atlas/"+vmDisk,
		"sudo udevadm settle",
		"? test -b "+vmDiskDev,
		// repair-if-wedged: no clone yet
		"? sudo dmsetup info "+vmCloneRoot,
		// nbd client, size-verified against the dest
		"sudo blockdev --getsize64 "+vmDiskDev,
		"- cat /sys/block/nbd0/pid",
		"- sudo nbd-client -d /dev/nbd0",
		"sudo nbd-client -N '' "+testSource+" 11165 /dev/nbd0 -persist",
		// dm-clone: metadata LV then the create
		"? sudo dmsetup info "+vmCloneRoot,
		"? sudo lvs --noheadings atlas/"+cloneMetaRoot,
		"? sudo lvs --noheadings atlas/"+cloneMetaRoot,
		"sudo lvcreate --type thin --thinpool atlas/pool0 -V 1G -n "+cloneMetaRoot+" atlas",
		"sudo lvchange -ay -K atlas/"+cloneMetaRoot,
		"sudo udevadm settle",
		"? test -b "+cloneMetaDev,
		"sudo dd if=/dev/zero of="+cloneMetaDev+" bs=1M count=16 conv=fsync",
		"sudo blockdev --getsize64 "+vmDiskDev,
		"sudo dmsetup create "+vmCloneRoot+" --table 0 20971520 clone "+cloneMetaDev+" "+vmDiskDev+" /dev/nbd0 32768",
	)
}

// An existing HEALTHY clone (live source client) is left alone, and the dm-clone is
// not rebuilt — idempotency that must not discard hydration progress.
func TestCloneTargetLeavesHealthyCloneAlone(t *testing.T) {
	fake := withCloneDeps(newFakeCommands()).
		exists("sudo lvs --noheadings atlas/"+vmDisk). // dest already exists
		exists("test -b "+vmDiskDev).
		exists("sudo dmsetup info "+vmCloneRoot). // clone present
		exists("sudo lvs --noheadings atlas/"+cloneMetaRoot).
		output("sudo blockdev --getsize64 "+vmDiskDev, "10737418240\n").
		output("cat /sys/block/nbd0/pid", "8123\n").
		exists("test -d /proc/8123").                                  // source client alive
		output("sudo blockdev --getsize64 /dev/nbd0", "10737418240\n") // connected size matches

	if _, err := CloneTarget(context.Background(), fake, testUUID, CloneTargetParams{ImageName: "debian12", DiskGB: 10, SourceHost: testSource}); err != nil {
		t.Fatalf("CloneTarget: %v", err)
	}
	// A healthy clone is neither removed nor recreated.
	assertNotIssued(t, fake, "dmsetup remove")
	assertNotIssued(t, fake, "dmsetup create")
	// A live, right-sized client is reused, not re-dialed.
	assertNotIssued(t, fake, "nbd-client -d")
	assertNotIssued(t, fake, "nbd-client -N")
}

// A dead source client under an existing clone is torn down so the stack rebuilds.
// The static fake models only the pre-remove state (a removal does not flip the
// device's presence), so the repair is asserted by the teardown it issues — the
// clone is removed and the dead nbd client is re-dialed; the rebuild create follows
// on a host where the removal actually took.
func TestCloneTargetRebuildsWedgedClone(t *testing.T) {
	fake := withCloneDeps(newFakeCommands()).
		exists("sudo lvs --noheadings atlas/"+vmDisk).
		exists("test -b "+vmDiskDev).
		exists("sudo dmsetup info "+vmCloneRoot). // clone present but source dead (no pid scripted)
		exists("test -b "+cloneMetaDev).
		output("sudo blockdev --getsize64 "+vmDiskDev, "10737418240\n")

	if _, err := CloneTarget(context.Background(), fake, testUUID, CloneTargetParams{ImageName: "debian12", DiskGB: 10, SourceHost: testSource}); err != nil {
		t.Fatalf("CloneTarget: %v", err)
	}
	// Wedge repair: the clone is removed and the dead client re-dialed.
	assertIssued(t, fake, "sudo dmsetup remove "+vmCloneRoot)
	assertIssued(t, fake, "sudo nbd-client -N '' "+testSource+" 11165 /dev/nbd0 -persist")
}

func TestCloneTargetDataDisk(t *testing.T) {
	fake := withCloneDeps(newFakeCommands()).
		exists("test -b "+vmDiskDev).
		exists("test -b "+cloneMetaDev).
		exists("test -b /dev/atlas/"+dataDisk).
		exists("test -b /dev/atlas/atlas-clonemeta-3f2504e0-4f89-41d3-9a0c-0305e82c3301-data").
		output("sudo blockdev --getsize64 "+vmDiskDev, "10737418240\n").
		output("sudo blockdev --getsize64 /dev/atlas/"+dataDisk, "21474836480\n")

	result, err := CloneTarget(context.Background(), fake, testUUID, CloneTargetParams{ImageName: "debian12", DiskGB: 10, DataDiskGB: 20, SourceHost: testSource})
	if err != nil {
		t.Fatalf("CloneTarget: %v", err)
	}
	if result.DataCloneDevice != "/dev/mapper/atlas-vm-3f2504e0-4f89-41d3-9a0c-0305e82c3301-data-clone" {
		t.Errorf("data clone device = %q", result.DataCloneDevice)
	}
	// The data disk gets its own thin LV, its own nbd client on port+1/slot+1, and its
	// own dm-clone.
	assertIssued(t, fake, "sudo lvcreate --type thin --thinpool atlas/pool0 -V 20G -n "+dataDisk+" atlas")
	assertIssued(t, fake, "sudo nbd-client -N '' "+testSource+" 11166 /dev/nbd1 -persist")
	assertIssued(t, fake, "sudo dmsetup create atlas-vm-3f2504e0-4f89-41d3-9a0c-0305e82c3301-data-clone")
}

func TestCloneTargetPreflightFailures(t *testing.T) {
	base := func() *fakeCommands { return withCloneDeps(newFakeCommands()) }
	cases := []struct {
		name   string
		mutate func(*fakeCommands)
	}{
		{"no nbd module", func(f *fakeCommands) { delete(f.present, "sudo modprobe nbd") }},
		{"no nbd-client", func(f *fakeCommands) { delete(f.present, "which nbd-client") }},
		{"no base image", func(f *fakeCommands) { delete(f.present, "sudo lvs --noheadings atlas/"+baseImage) }},
		{"no image dir", func(f *fakeCommands) { delete(f.present, "test -d "+imageDir) }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := base()
			testCase.mutate(fake)
			if _, err := CloneTarget(context.Background(), fake, testUUID, CloneTargetParams{ImageName: "debian12", DiskGB: 10, SourceHost: testSource}); err == nil {
				t.Fatal("CloneTarget accepted a failed pre-flight")
			}
			assertNotIssued(t, fake, "lvcreate")
			assertNotIssued(t, fake, "nbd-client -N")
		})
	}
}

// A pool past the hydration threshold refuses before creating any destination LV.
func TestCloneTargetRefusesFullPool(t *testing.T) {
	fake := withCloneDeps(newFakeCommands()).
		output("sudo lvs --noheadings -o data_percent atlas/pool0", "85.00")
	if _, err := CloneTarget(context.Background(), fake, testUUID, CloneTargetParams{ImageName: "debian12", DiskGB: 10, SourceHost: testSource}); err == nil {
		t.Fatal("CloneTarget accepted a pool past the hydration threshold")
	}
	assertNotIssued(t, fake, "lvcreate")
}
