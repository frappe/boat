package image

import (
	"context"
	"strconv"
)

// downloadRootfs fetches the source squashfs, verifies it against RootfsSHA256
// (the digest of the *source squashfs*, not the ext4 we later build from it), and
// unsquashes it into a scratch directory. Returns the extracted directory path;
// the caller normalizes it and builds the ext4.
func downloadRootfs(ctx context.Context, cmd commands, params SyncImageParams) (string, error) {
	squashfsPath := "/tmp/atlas-" + params.ImageName + ".squashfs"
	extractedDirectory := "/tmp/atlas-" + params.ImageName + "-rootfs"
	if _, err := cmd.Run(ctx, "sudo rm -f {} {}", squashfsPath+".part", squashfsPath); err != nil {
		return "", err
	}
	if _, err := cmd.Run(ctx, "sudo rm -rf {}", extractedDirectory); err != nil {
		return "", err
	}

	if _, err := cmd.Run(ctx, "sudo curl -fsSL --output {} {}", squashfsPath+".part", params.RootfsURL); err != nil {
		return "", err
	}
	if _, err := cmd.Input(ctx, params.RootfsSHA256+"  "+squashfsPath+".part", "sudo sha256sum -c -"); err != nil {
		return "", err
	}
	if _, err := cmd.Run(ctx, "sudo mv {} {}", squashfsPath+".part", squashfsPath); err != nil {
		return "", err
	}
	if _, err := cmd.Run(ctx, "sudo unsquashfs -d {} {}", extractedDirectory, squashfsPath); err != nil {
		return "", err
	}
	return extractedDirectory, nil
}

// installGuestNetworkUnit installs the in-guest atlas-network.service (static
// IPv6 bring-up) and its placeholder env file into the extracted rootfs, wired
// into multi-user.target.wants so it starts at boot. The unit itself comes from a
// server path (a real file copy via install(1)), not inline content.
func installGuestNetworkUnit(ctx context.Context, cmd commands, params SyncImageParams, root string) error {
	if err := cmd.InstallDirectory(ctx, root+"/etc/systemd/system", "0755"); err != nil {
		return err
	}
	if err := cmd.InstallDirectory(ctx, root+"/etc/systemd/system/multi-user.target.wants", "0755"); err != nil {
		return err
	}
	if _, err := cmd.Run(
		ctx, "sudo install -m 0644 {} {}",
		params.GuestNetworkUnit, root+"/etc/systemd/system/atlas-network.service",
	); err != nil {
		return err
	}
	if _, err := cmd.Run(
		ctx, "sudo ln -sf /etc/systemd/system/atlas-network.service {}",
		root+"/etc/systemd/system/multi-user.target.wants/atlas-network.service",
	); err != nil {
		return err
	}
	return cmd.InstallFile(ctx, "", root+"/etc/atlas-network.env", "0644")
}

// buildExt4 turns the normalized rootfs directory into a pristine ext4 of diskGB,
// labelled atlas-root to match the /etc/fstab written in normalizeRootfs.
//
// metadata_csum_seed decouples the per-block checksum seed from the filesystem
// UUID. Without it, `tune2fs -U random` (run per-VM in prepare_lv to give each
// clone a distinct UUID) must rewrite every metadata block's checksum; on a CoW
// thin snapshot each such write forces a pool copy, costing ~1.7s per provision.
// With the seed baked into the base image, the UUID change is a single superblock
// write (~9ms) on every snapshot. Measured 185x: 1.673s -> 0.009s.
func buildExt4(ctx context.Context, cmd commands, root, rootfsPath string, diskGB int) error {
	// Do NOT blanket `chown -R root:root` the tree here. `sudo unsquashfs` restored
	// the source squashfs's uid/gid faithfully and `mkfs.ext4 -d` copies them
	// through, so the reconstituted ext4 already carries the correct ownership. A
	// blanket flatten destroys every legitimately non-root path — most damagingly a
	// promoted image's /home/frappe (uid 1000) when that image is re-served as a
	// squashfs and fanned out via sync-image: a server booting the distributed
	// image then fails with `cd /home/frappe/pilot: Permission denied`. (It also
	// clobbered the man cache below, which is why that fix-up existed at all.)

	// Ensure the man cache is man-owned (Ubuntu: uid 6 / gid 12). Ubuntu installs
	// `mandb` SETUID `man`, so the dpkg man-db trigger runs as the `man` user, and
	// systemd-tmpfiles ships `d /var/cache/man 0755 man man` to make the cache
	// man-owned for exactly that reason. tmpfiles never runs at build time, so a
	// source that stored the cache root-owned would make every guest `apt` flood
	// `mandb: can't chmod /var/cache/man/<locale>/CACHEDIR.TAG: Operation not
	// permitted` (harmless, but noisy enough to push apt past the Task timeout) —
	// this keeps it correct regardless of the source. Use the NUMERIC id: this runs
	// on the host against a foreign rootfs, so a by-name `man:man` would resolve
	// against the HOST's /etc/passwd. Guard on existence: the Ubuntu MINIMAL image
	// ships no /var/cache/man, so an unconditional chown aborts the whole sync
	// there; `[ -d ] &&` makes it a documented no-op, matching the guarded-strip
	// convention in normalizeRootfs.
	manCache := root + "/var/cache/man"
	if _, err := cmd.Shell(ctx, "[ -d {} ] && chown -R 6:12 {} || true", manCache, manCache); err != nil {
		return err
	}

	if _, err := cmd.Run(ctx, "sudo truncate -s {} {}", strconv.Itoa(diskGB)+"G", rootfsPath+".part"); err != nil {
		return err
	}
	if _, err := cmd.Run(
		ctx, "sudo mkfs.ext4 -q -O metadata_csum_seed -L atlas-root -d {} -F {}", root, rootfsPath+".part",
	); err != nil {
		return err
	}
	_, err := cmd.Run(ctx, "sudo mv {} {}", rootfsPath+".part", rootfsPath)
	return err
}
