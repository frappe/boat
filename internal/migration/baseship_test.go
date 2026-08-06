package migration

import (
	"context"
	"testing"
)

const (
	baseImageDev     = "/dev/atlas/atlas-image-debian12"
	baseCloneNm      = "atlas-base-debian12-clone"
	baseCloneMetaNm  = "atlas-clonemeta-base-debian12"
	baseCloneMetaDev = "/dev/atlas/atlas-clonemeta-base-debian12"
	baseTarPath      = "/var/lib/atlas/run/migrate-base-meta-debian12.tar"
	basePidFile      = "/var/lib/atlas/run/migrate-nbd-11167.pid"
	metaPidFile      = "/var/lib/atlas/run/migrate-nbd-11168.pid"
)

func TestExportBase(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo lvs --noheadings atlas/"+baseImage).
		exists("test -d "+imageDir).
		exists("test -b "+baseImageDev).
		exists("sudo test -f "+basePidFile).
		exists("sudo test -f "+metaPidFile).
		output("sudo cat "+basePidFile, "5000\n").
		output("sudo cat "+metaPidFile, "5001\n").
		output("sudo stat -c %s "+baseTarPath, "4096\n").
		output("sudo blockdev --getsize64 "+baseImageDev, "8589934592\n")

	result, err := ExportBase(context.Background(), fake, testUUID, ExportBaseParams{ImageName: "debian12", BindAddress: testBindIPv4})
	if err != nil {
		t.Fatalf("ExportBase: %v", err)
	}
	want := ExportBaseResult{NBDPort: 11167, NBDPID: 5000, BaseSizeBytes: 8589934592, MetaPort: 11168, MetaPID: 5001, MetaSizeBytes: 4096}
	if result != want {
		t.Errorf("result = %+v, want %+v", result, want)
	}

	assertTrace(t, fake,
		"? sudo lvs --noheadings atlas/"+baseImage,
		"? test -d "+imageDir,
		// activate the immutable base LV so qemu-nbd opens a live node
		"sudo lvchange -ay -K atlas/"+baseImage,
		"sudo udevadm settle",
		"? test -b "+baseImageDev,
		"sudo mkdir -p /var/lib/atlas/run",
		// 1. base rootfs LV over NBD on port+2
		"- ss -ltn",
		"sudo qemu-nbd --persistent --read-only --cache=none --bind=203.0.113.7 --port=11167 --pid-file="+basePidFile+" --fork "+baseImageDev,
		"? sudo test -f "+basePidFile,
		"sudo cat "+basePidFile,
		// 2. image-dir tar staged then served on port+3
		"- ss -ltn",
		"sudo tar -cf "+baseTarPath+" -C "+imageDir+" .",
		"sudo stat -c %s "+baseTarPath,
		"- ss -ltn",
		"sudo qemu-nbd --persistent --read-only --cache=none --bind=203.0.113.7 --port=11168 --pid-file="+metaPidFile+" --fork "+baseTarPath,
		"? sudo test -f "+metaPidFile,
		"sudo cat "+metaPidFile,
		"sudo blockdev --getsize64 "+baseImageDev,
	)
}

func TestReceiveBasePrepare(t *testing.T) {
	fake := withHealthyPool(newFakeCommands()).
		exists("sudo modprobe nbd").
		exists("sudo modprobe dm_clone").
		exists("which nbd-client").
		exists("test -b "+baseImageDev).
		exists("test -b "+baseCloneMetaDev).
		output("sudo blockdev --getsize64 "+baseImageDev, "8589934592\n")

	err := ReceiveBase(context.Background(), fake, testUUID, ReceiveBaseParams{ImageName: "debian12", DiskGB: 8, SourceHost: testSource, Phase: "prepare"})
	if err != nil {
		t.Fatalf("ReceiveBase prepare: %v", err)
	}

	assertTrace(t, fake,
		// already-received? base LV absent, so on we go
		"? sudo lvs --noheadings atlas/"+baseImage,
		// deps + pool
		"? sudo modprobe nbd",
		"? sudo modprobe dm_clone",
		"? which nbd-client",
		"sudo lvs --noheadings -o data_percent atlas/pool0",
		"sudo lvs --noheadings -o metadata_percent atlas/pool0",
		// writable dest thin LV named as the final base image
		"? sudo lvs --noheadings atlas/"+baseImage,
		"sudo lvcreate --type thin --thinpool atlas/pool0 -V 8G -n "+baseImage+" atlas",
		"sudo lvchange -ay -K atlas/"+baseImage,
		"sudo udevadm settle",
		"? test -b "+baseImageDev,
		// repair-if-wedged: no clone yet
		"? sudo dmsetup info "+baseCloneNm,
		// nbd client to the source base export on port+2 / slot+2
		"sudo blockdev --getsize64 "+baseImageDev,
		"? test -e /sys/block/nbd2/pid",
		"- sudo nbd-client -d /dev/nbd2",
		"sudo nbd-client "+testSource+" 11167 /dev/nbd2 -persist",
		// dm-clone over the base
		"? sudo dmsetup info "+baseCloneNm,
		"? sudo lvs --noheadings atlas/"+baseCloneMetaNm,
		"? sudo lvs --noheadings atlas/"+baseCloneMetaNm,
		"sudo lvcreate --type thin --thinpool atlas/pool0 -V 1G -n "+baseCloneMetaNm+" atlas",
		"sudo lvchange -ay -K atlas/"+baseCloneMetaNm,
		"sudo udevadm settle",
		"? test -b "+baseCloneMetaDev,
		"sudo dd if=/dev/zero of="+baseCloneMetaDev+" bs=1M count=16 conv=fsync",
		"sudo blockdev --getsize64 "+baseImageDev,
		"sudo dmsetup create "+baseCloneNm+" --table 0 16777216 clone "+baseCloneMetaDev+" "+baseImageDev+" /dev/nbd2 32768",
		// image directory: absent, so pull the tar over the meta export (port+3 / slot+3)
		"? test -d "+imageDir,
		"? test -e /sys/block/nbd3/pid",
		"- sudo nbd-client -d /dev/nbd3",
		"sudo nbd-client "+testSource+" 11168 /dev/nbd3 -persist",
		"sudo install -d -m 0700 "+imageDir,
		"$ sudo tar -xf /dev/nbd3 -C "+imageDir,
	)
}

// A base already present and read-only is a no-op on either phase — a prior finalize
// completed.
func TestReceiveBaseAlreadyDone(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo lvs --noheadings atlas/"+baseImage).
		output("sudo lvs --noheadings -o lv_attr atlas/"+baseImage, "-ri-------\n") // 'r' at index 1

	if err := ReceiveBase(context.Background(), fake, testUUID, ReceiveBaseParams{ImageName: "debian12", DiskGB: 8, SourceHost: testSource, Phase: "prepare"}); err != nil {
		t.Fatalf("ReceiveBase: %v", err)
	}
	// Nothing is built on a base that is already a finished local image.
	assertNotIssued(t, fake, "modprobe")
	assertNotIssued(t, fake, "lvcreate")
	assertNotIssued(t, fake, "dmsetup create")
}

func TestReceiveBaseFinalize(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo lvs --noheadings atlas/"+baseImage).
		output("sudo lvs --noheadings -o lv_attr atlas/"+baseImage, "-wi-------\n"). // writable → proceed
		exists("sudo dmsetup info "+baseCloneNm).
		exists("sudo lvs --noheadings atlas/"+baseCloneMetaNm).
		output("sudo dmsetup status "+baseCloneNm, "0 16777216 clone 8/1024 32768 512/512 0 rw")

	if err := ReceiveBase(context.Background(), fake, testUUID, ReceiveBaseParams{ImageName: "debian12", DiskGB: 8, SourceHost: testSource, Phase: "finalize"}); err != nil {
		t.Fatalf("ReceiveBase finalize: %v", err)
	}
	// A fully-hydrated base clone is removed outright (not transparently collapsed — the
	// base is never booted from), the base flipped read-only, and both clients dropped.
	assertIssued(t, fake, "sudo dmsetup remove "+baseCloneNm)
	assertIssued(t, fake, "sudo lvremove -f atlas/"+baseCloneMetaNm)
	assertIssued(t, fake, "sudo lvchange --permission r atlas/"+baseImage)
	assertIssued(t, fake, "sudo nbd-client -d /dev/nbd2")
	assertIssued(t, fake, "sudo nbd-client -d /dev/nbd3")
}

// Finalize refuses to collapse a base clone that is not fully hydrated.
func TestReceiveBaseFinalizeRefusesPartial(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo dmsetup info "+baseCloneNm).
		output("sudo dmsetup status "+baseCloneNm, "0 16777216 clone 8/1024 32768 256/512 0 rw")

	if err := ReceiveBase(context.Background(), fake, testUUID, ReceiveBaseParams{ImageName: "debian12", DiskGB: 8, SourceHost: testSource, Phase: "finalize"}); err == nil {
		t.Fatal("finalize collapsed a half-hydrated base")
	}
	assertNotIssued(t, fake, "dmsetup remove")
}

func TestReceiveBaseRejectsUnknownPhase(t *testing.T) {
	fake := newFakeCommands()
	if err := ReceiveBase(context.Background(), fake, testUUID, ReceiveBaseParams{ImageName: "debian12", Phase: "wibble"}); err == nil {
		t.Fatal("ReceiveBase accepted an unknown phase")
	}
}
