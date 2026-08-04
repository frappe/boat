package image

import (
	"context"
	"testing"
)

// The derived paths for one image, spelled out so a golden reads like a host's
// journal and a drifted template fails visibly. sync-image.py is the oracle.
const (
	syncImageName  = "ubuntu-24.04"
	syncImageDir   = "/var/lib/atlas/images/ubuntu-24.04"
	syncKernelName = "vmlinux-6.1.141"
	syncKernelPath = syncImageDir + "/" + syncKernelName
	syncPackedPath = syncKernelPath + ".vmlinuz"
	syncKernelURL  = "https://images.example/vmlinuz.zst"
	syncKernelSHA  = "1111111111kernel"
	syncRootfsName = "ubuntu-24.04.ext4"
	syncRootfsPath = syncImageDir + "/" + syncRootfsName
	syncRootfsURL  = "https://images.example/ubuntu-24.04.squashfs"
	syncRootfsSHA  = "2222222222rootfs"
	syncDiskGB     = 28
	syncExtracted  = "/tmp/atlas-ubuntu-24.04-rootfs"
	syncSquashfs   = "/tmp/atlas-ubuntu-24.04.squashfs"

	syncManifestURL = "https://images.example/ubuntu-24.04.manifest"
	syncKver        = "6.8.0-117-generic"
	syncWork        = "/tmp/atlas-modules-6.8.0-117-generic"
	syncSourceMods  = syncWork + "/deb/lib/modules/" + syncKver
	syncDeb         = syncWork + "/linux-modules-6.8.0-117-generic_6.8.0-117.117_amd64.deb"
	syncVirtioSrc   = syncSourceMods + "/kernel/drivers/char/hw_random/virtio-rng.ko.zst"

	syncBaseLV     = "atlas-image-ubuntu-24.04"
	syncBaseDevice = "/dev/atlas/atlas-image-ubuntu-24.04"

	// The zstd magic sits at hex-nibble 512 -> byte 256 -> `tail -c +257`.
	syncHexOffsetShell = "xxd -p " + syncPackedPath + " | tr -d '\\n' | grep -bo '28b52ffd' | head -1 | cut -d: -f1"
	syncELFMagicShell  = "head -c 4 " + syncKernelPath + ".part | xxd -p"

	// A two-column manifest (tab-separated); only the linux-modules-*-generic line
	// pins the kernel version.
	syncManifest = "adduser\t3.137ubuntu1\n" +
		"linux-modules-6.8.0-117-generic\t6.8.0-117.117\n" +
		"zlib1g\t1:1.3.dfsg\n"
)

func syncParams() SyncImageParams {
	return SyncImageParams{
		ImageName:      syncImageName,
		KernelURL:      syncKernelURL,
		KernelFilename: syncKernelName,
		KernelSHA256:   syncKernelSHA,
		RootfsURL:      syncRootfsURL,
		RootfsFilename: syncRootfsName,
		RootfsSHA256:   syncRootfsSHA,
		DefaultDiskGB:  syncDiskGB,
		// GuestNetworkUnit left empty on purpose, to prove the staged default is applied.
	}
}

// newSyncFake scripts a host where nothing is built yet but every read the build
// makes has an answer: the kernel magic offsets, the module manifest and the deb
// layout, and the base LV coming up as a block device after activation.
func newSyncFake() *fakeCommands {
	return newFakeCommands().
		output(syncHexOffsetShell, "512\n").
		output(syncELFMagicShell, "7f454c46\n").
		output("curl -fsSL "+syncManifestURL, syncManifest).
		output("ls "+syncWork+"/linux-modules-"+syncKver+"_*.deb", syncDeb+"\n").
		output("find "+syncSourceMods+" -name virtio-rng.ko* | head -1", syncVirtioSrc+"\n").
		output("find "+syncSourceMods+"/kernel/zfs -name zfs.ko* | head -1", syncSourceMods+"/kernel/zfs/zfs.ko.zst\n").
		exists("test -b " + syncBaseDevice)
}

func TestSyncImageHappyPath(t *testing.T) {
	fake := newSyncFake()

	result, err := SyncImage(context.Background(), fake, syncParams())
	if err != nil {
		t.Fatalf("SyncImage: %v", err)
	}
	want := SyncImageResult{ImageName: syncImageName, BaseImageLV: syncBaseLV}
	if result != want {
		t.Errorf("result = %+v, want %+v", result, want)
	}

	// Kernel: download the packed artifact, verify the packed digest, decompress the
	// zstd frame at +257, confirm the ELF magic.
	assertIssued(t, fake, "installdir 0700 "+syncImageDir)
	assertIssued(t, fake, "sudo curl -fsSL --output "+syncPackedPath+".part "+syncKernelURL)
	assertIssued(t, fake, "< "+syncKernelSHA+"  "+syncPackedPath+".part | sudo sha256sum -c -")
	assertIssued(t, fake, "$ tail -c +257 "+syncPackedPath+" | zstd -dc -f > "+syncKernelPath+".part")
	assertIssued(t, fake, "$ "+syncELFMagicShell)

	// Rootfs: download the squashfs, verify its digest, unsquash it.
	assertIssued(t, fake, "sudo curl -fsSL --output "+syncSquashfs+".part "+syncRootfsURL)
	assertIssued(t, fake, "< "+syncRootfsSHA+"  "+syncSquashfs+".part | sudo sha256sum -c -")
	assertIssued(t, fake, "sudo unsquashfs -d "+syncExtracted+" "+syncSquashfs)

	// Guest network unit: the empty GuestNetworkUnit defaulted to the staged sidecar.
	assertIssued(t, fake, "sudo install -m 0644 "+StagedGuestNetworkUnit+" "+syncExtracted+"/etc/systemd/system/atlas-network.service")

	// Normalize: a boot-blocker mask, a junk-unit mask, the ssh-key glob, resolv.conf.
	assertIssued(t, fake, "sudo ln -sf /dev/null "+syncExtracted+"/etc/systemd/system/cloud-init.service")
	assertIssued(t, fake, "sudo ln -sf /dev/null "+syncExtracted+"/etc/systemd/system/apparmor.service")
	assertIssued(t, fake, "$ rm -f "+syncExtracted+"/etc/ssh/ssh_host_*_key "+syncExtracted+"/etc/ssh/ssh_host_*_key.pub")
	assertInstalled(t, fake, syncExtracted+"/etc/resolv.conf", "nameserver 2606:4700:4700::1111\n")
	assertInstalled(t, fake, syncExtracted+"/etc/hosts", hostsContent)

	// Modules: kver from the manifest, virtio_rng copied by name, zfs subtree, depmod,
	// the eager-load pin.
	assertIssued(t, fake, "$ cd "+syncWork+" && apt-get download linux-modules-"+syncKver)
	assertIssued(t, fake, "sudo dpkg-deb -x "+syncDeb+" "+syncWork+"/deb")
	assertIssued(t, fake, "sudo cp "+syncVirtioSrc+" "+syncExtracted+"/lib/modules/"+syncKver+"/kernel/drivers/char/hw_random/virtio-rng.ko.zst")
	assertIssued(t, fake, "sudo cp -a "+syncSourceMods+"/kernel/zfs "+syncExtracted+"/lib/modules/"+syncKver+"/kernel/zfs")
	assertIssued(t, fake, "sudo depmod -b "+syncExtracted+" "+syncKver)
	assertIssued(t, fake, "install 0644 "+syncExtracted+"/etc/modules-load.d/atlas-guest-modules.conf")

	// ext4: the checksum-seeded mkfs against the normalized tree.
	assertIssued(t, fake, "sudo mkfs.ext4 -q -O metadata_csum_seed -L atlas-root -d "+syncExtracted+" -F "+syncRootfsPath+".part")
	assertIssued(t, fake, "sudo mv "+syncRootfsPath+".part "+syncRootfsPath)

	// Base LV import: create the thin volume, dd the ext4 in, flip it read-only.
	assertIssued(t, fake, "sudo lvcreate --type thin --thinpool atlas/pool0 -V 28G -n "+syncBaseLV+" atlas")
	assertIssued(t, fake, "sudo dd if="+syncRootfsPath+" of="+syncBaseDevice+" bs=4M conv=fsync status=none")
	assertIssued(t, fake, "sudo lvchange --permission r atlas/"+syncBaseLV)
}

// A final ext4 already present means the image is complete: SyncImage stat-probes
// the kernel (present) and the rootfs (present) and returns, touching nothing else.
func TestSyncImageShortCircuitsWhenRootfsBuilt(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo test -f " + syncKernelPath).
		exists("sudo test -f " + syncRootfsPath)

	result, err := SyncImage(context.Background(), fake, syncParams())
	if err != nil {
		t.Fatalf("SyncImage: %v", err)
	}
	if want := (SyncImageResult{ImageName: syncImageName}); result != want {
		t.Errorf("result = %+v, want %+v", result, want)
	}

	assertTrace(t, fake,
		"installdir 0700 "+syncImageDir,
		"? sudo test -f "+syncKernelPath,
		"? sudo test -f "+syncRootfsPath,
	)
	assertNotIssued(t, fake, "curl")
	assertNotIssued(t, fake, "unsquashfs")
	assertNotIssued(t, fake, "lvcreate")
}

// A kernel already present skips only the kernel download; the rootfs build still
// runs to completion (this is per-artifact idempotency, not a whole-verb one).
func TestSyncImageSkipsKernelWhenPresent(t *testing.T) {
	fake := newSyncFake().exists("sudo test -f " + syncKernelPath)

	result, err := SyncImage(context.Background(), fake, syncParams())
	if err != nil {
		t.Fatalf("SyncImage: %v", err)
	}
	if want := (SyncImageResult{ImageName: syncImageName, BaseImageLV: syncBaseLV}); result != want {
		t.Errorf("result = %+v, want %+v", result, want)
	}

	// No kernel artifact was fetched or decompressed.
	assertNotIssued(t, fake, "curl -fsSL --output "+syncPackedPath+".part")
	assertNotIssued(t, fake, "zstd -dc -f")
	// But the rootfs half ran in full.
	assertIssued(t, fake, "sudo unsquashfs -d "+syncExtracted)
	assertIssued(t, fake, "sudo mkfs.ext4 -q -O metadata_csum_seed")
	assertIssued(t, fake, "sudo lvcreate --type thin --thinpool atlas/pool0 -V 28G -n "+syncBaseLV+" atlas")
}
