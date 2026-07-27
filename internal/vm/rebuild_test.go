package vm

import (
	"context"
	"strings"
	"testing"
)

const (
	testImage              = "ubuntu-24.04"
	testMountPoint         = "/tmp/atlas-mount-abc123"
	testHostname           = "atlas-11111111"
	testMachineID          = "11111111222233334444555555555555"
	testRootDevice         = "/dev/atlas/atlas-vm-" + testUUID
	testDataDevice         = "/dev/atlas/atlas-data-" + testUUID
	testDataSnapshotDevice = "/dev/atlas/atlas-datasnap-99999999"
)

var testIdentity = Identity{
	IPv6Address:     "2604:a880:800:c1::1",
	IPv4GuestCIDR:   "10.201.0.2/30",
	IPv4Gateway:     "10.201.0.1",
	SSHPublicKey:    "ssh-ed25519 AAAAC3Nz",
	DataDiskMountAt: "/data",
}

// aRebuiltHost answers what the rebuild reads back: an origin and a jail that
// are there, a root volume that exists before the swap and not after it, a
// mount point, and a device number for the new jail node.
func aRebuiltHost(fake *fakeCommands) {
	fake.reply("sudo lvs --noheadings atlas/atlas-vm-"+testUUID, true, false)
	// A pristine image rootfs carries no data-disk line in its fstab yet.
	fake.reply("sudo grep -q LABEL=atlas-data "+testMountPoint+"/etc/fstab", false)
	fake.output("sudo mktemp -d /tmp/atlas-mount-XXXXXX", testMountPoint+"\n")
	fake.output("lsblk -ndo MAJ:MIN "+testRootDevice, "252:5  \n")
	fake.output("lsblk -ndo MAJ:MIN "+testDataDevice, "252:6  \n")
}

func TestRebuildReplacesTheRootFilesystemFromAnImage(t *testing.T) {
	files := testFiles(testUUID)
	fake := newFakeCommands()
	aRebuiltHost(fake)
	request := RebuildRequest{
		Image: testImage, DiskGB: 40, FirecrackerUID: testFirecrackerUID, Identity: testIdentity,
	}

	if err := newTestManager(fake).Rebuild(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	expected := []string{
		"? sudo test -d " + files.jailRoot,
		"? sudo lvs --noheadings atlas/atlas-image-" + testImage,
		"sudo rm -rf " + files.memorySnapshotDirectory,
		// The old volume goes first: the snapshot below is idempotent and would
		// otherwise keep the disk that is already there.
		"? sudo lvs --noheadings atlas/atlas-vm-" + testUUID,
		"sudo lvremove -f atlas/atlas-vm-" + testUUID,
		"? sudo lvs --noheadings atlas/atlas-vm-" + testUUID,
		"sudo lvcreate -s atlas/atlas-image-" + testImage + " -n atlas-vm-" + testUUID,
		"sudo lvchange -ay -K atlas/atlas-vm-" + testUUID,
		"sudo udevadm settle",
		"? test -b " + testRootDevice,
		"- sudo lvextend -r -L 40G " + testRootDevice,
		"sudo tune2fs -U random -L atlas-root " + testRootDevice,
	}
	expected = append(expected, identityCommands()...)
	expected = append(expected,
		"lsblk -ndo MAJ:MIN "+testRootDevice,
		"sudo rm -f "+files.rootFilesystemNode,
		"sudo mknod "+files.rootFilesystemNode+" b 252 5",
		"sudo chown 1200:1200 "+files.rootFilesystemNode,
		"sudo chmod 0660 "+files.rootFilesystemNode,
	)
	assertTrace(t, fake, expected...)
}

func TestRebuildRestoresFromASnapshotInsteadOfAnImage(t *testing.T) {
	fake := newFakeCommands()
	aRebuiltHost(fake)
	request := RebuildRequest{
		SnapshotDevice: "/dev/atlas/atlas-snap-77777777", Image: testImage,
		DiskGB: 40, FirecrackerUID: testFirecrackerUID, Identity: testIdentity,
	}

	if err := newTestManager(fake).Rebuild(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if countTrace(fake, "sudo lvcreate -s atlas/atlas-snap-77777777 -n atlas-vm-"+testUUID) != 1 {
		t.Errorf("the snapshot did not win over the image:\n  %s", strings.Join(fake.trace, "\n  "))
	}
	for _, line := range fake.trace {
		if strings.Contains(line, "atlas-image-") {
			t.Errorf("a restore looked at an image: %s", line)
		}
	}
}

// A rebuild from an image has no source for the data disk, and wiping a
// tenant's data because they asked to reinstall the OS is not a default.
func TestRebuildLeavesTheDataDiskAloneWithoutADataSnapshot(t *testing.T) {
	fake := newFakeCommands()
	aRebuiltHost(fake)
	request := RebuildRequest{
		Image: testImage, DiskGB: 40, FirecrackerUID: testFirecrackerUID, Identity: testIdentity,
	}

	if err := newTestManager(fake).Rebuild(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	for _, line := range fake.trace {
		if strings.Contains(line, "atlas-data-"+testUUID) {
			t.Errorf("the data disk was touched: %s", line)
		}
	}
}

func TestRebuildRestoresTheDataDiskFromItsOwnSnapshot(t *testing.T) {
	files := testFiles(testUUID)
	fake := newFakeCommands()
	aRebuiltHost(fake)
	fake.reply("sudo lvs --noheadings atlas/atlas-data-"+testUUID, true, false)
	request := RebuildRequest{
		Image: testImage, DiskGB: 40, FirecrackerUID: testFirecrackerUID, Identity: testIdentity,
		DataSnapshotDevice: testDataSnapshotDevice, DataDiskGB: 100,
	}

	if err := newTestManager(fake).Rebuild(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	tail := fake.trace[len(fake.trace)-16:]
	assertLines(t, tail,
		"? sudo lvs --noheadings atlas/atlas-datasnap-99999999",
		"? sudo lvs --noheadings atlas/atlas-data-"+testUUID,
		"sudo lvremove -f atlas/atlas-data-"+testUUID,
		"? sudo lvs --noheadings atlas/atlas-data-"+testUUID,
		"sudo lvcreate -s atlas/atlas-datasnap-99999999 -n atlas-data-"+testUUID,
		"sudo lvchange -ay -K atlas/atlas-data-"+testUUID,
		"sudo udevadm settle",
		"? test -b "+testDataDevice,
		"- sudo lvextend -r -L 100G "+testDataDevice,
		"- sudo e2fsck -fy "+testDataDevice,
		// The UUID is rerolled and the LABEL kept: the guest mounts the data disk
		// by LABEL=atlas-data, and the fstab line above has to keep matching.
		"sudo tune2fs -U random -L atlas-data "+testDataDevice,
		"lsblk -ndo MAJ:MIN "+testDataDevice,
		"sudo rm -f "+files.dataNode,
		"sudo mknod "+files.dataNode+" b 252 6",
		"sudo chown 1200:1200 "+files.dataNode,
		"sudo chmod 0660 "+files.dataNode,
	)
}

func TestRebuildRefusesASourceThisHostDoesNotHave(t *testing.T) {
	files := testFiles(testUUID)
	for name, request := range map[string]RebuildRequest{
		"absent image":    {Image: testImage, DiskGB: 40},
		"absent snapshot": {SnapshotDevice: "/dev/atlas/atlas-snap-77777777", DiskGB: 40},
		"no source":       {DiskGB: 40},
	} {
		fake := newFakeCommands()
		fake.reply("sudo lvs --noheadings atlas/atlas-image-"+testImage, false)
		fake.reply("sudo lvs --noheadings atlas/atlas-snap-77777777", false)

		err := newTestManager(fake).Rebuild(context.Background(), nil, testUUID, request)

		if err == nil {
			t.Fatalf("%s: Rebuild succeeded, want a refusal", name)
		}
		// Refused before the volume is dropped: a rebuild that removed the disk
		// and then found no source would leave the VM with nothing at all.
		if countTrace(fake, "sudo lvremove -f atlas/atlas-vm-"+testUUID) != 0 {
			t.Errorf("%s: the old volume was removed anyway", name)
		}
		if fake.trace[0] != "? sudo test -d "+files.jailRoot {
			t.Errorf("%s: first command was %q", name, fake.trace[0])
		}
	}
}

func TestRebuildRefusesAVirtualMachineWithNoJail(t *testing.T) {
	files := testFiles(testUUID)
	fake := newFakeCommands()
	fake.reply("sudo test -d "+files.jailRoot, false)

	err := newTestManager(fake).Rebuild(
		context.Background(), nil, testUUID, RebuildRequest{Image: testImage, DiskGB: 40},
	)

	if err == nil {
		t.Fatal("Rebuild succeeded with no jail, want a refusal")
	}
	assertTrace(t, fake, "? sudo test -d "+files.jailRoot)
}

// assertLines compares a slice of the trace, for the verbs long enough that a
// golden sequence of the whole thing would hide what a test is about.
func assertLines(t *testing.T, got []string, expected ...string) {
	t.Helper()
	if len(got) != len(expected) {
		t.Fatalf("commands:\ngot:\n  %s\nwant:\n  %s",
			strings.Join(got, "\n  "), strings.Join(expected, "\n  "))
	}
	for index := range expected {
		if got[index] != expected[index] {
			t.Errorf("command %d:\ngot:  %s\nwant: %s", index, got[index], expected[index])
		}
	}
}
