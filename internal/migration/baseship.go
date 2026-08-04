package migration

import (
	"context"
	"strconv"
	"strings"
)

// The local-base-image ship (spec/24 §5): a base image promoted from a snapshot has
// no rootfs URL, so sync-image cannot place it on the target — it lives only on the
// host it was promoted on, and a VM on it could not otherwise migrate. This ships the
// local base the same way the disk is shipped — an NBD export the target hydrates
// into a fresh LV — over the SAME plain-TCP channel, on the migration's +2/+3 ports
// (root disk = base, data = base+1, so +2/+3 never collide). The base LV is READ-ONLY
// and immutable, so it is exported DIRECTLY with no snapshot in between.

// ExportBaseParams is what the source cannot derive: the image to ship and the
// address qemu-nbd binds. The ports are DERIVED (base = NBDPort+2, image-dir tar =
// NBDPort+3).
type ExportBaseParams struct {
	ImageName   string
	BindAddress string
}

// ExportBaseResult carries both exports back: the base rootfs LV and the image-dir
// tar (kernel + rootfs sentinel), each with its port, pid and size.
type ExportBaseResult struct {
	NBDPort       int
	NBDPID        int
	BaseSizeBytes int64
	MetaPort      int
	MetaPID       int
	MetaSizeBytes int64
}

// ExportBase serves a VM's read-only base image LV plus a tar of its image directory
// over NBD, so a target with neither can gain both a base image needs (the rootfs LV
// AND the on-disk kernel) over one channel. Idempotent: reuses already-serving
// qemu-nbd processes and the staged tar. Ports scripts/migration-export-base.py.
func ExportBase(ctx context.Context, cmd commands, uuid string, params ExportBaseParams) (ExportBaseResult, error) {
	port, err := NBDPort(uuid)
	if err != nil {
		return ExportBaseResult{}, err
	}
	basePort := port + 2
	metaPort := port + 3

	if !lvExists(ctx, cmd, baseImageLV(params.ImageName)) {
		return ExportBaseResult{}, errBaseImageMissing(params.ImageName)
	}
	if !cmd.OK(ctx, "test -d {}", imageDirectory(params.ImageName)) {
		return ExportBaseResult{}, errImageDirectoryMissing(params.ImageName)
	}
	// Activate so qemu-nbd opens a live block device (a skip-flagged LV would be a
	// missing node). The base is the source of truth and immutable — exported read-only.
	if err := activate(ctx, cmd, baseImageLV(params.ImageName)); err != nil {
		return ExportBaseResult{}, err
	}
	if _, err := cmd.Run(ctx, "sudo mkdir -p {}", runDirectory); err != nil {
		return ExportBaseResult{}, err
	}

	// 1. The rootfs LV over NBD (block export).
	basePID, err := ensureNBDExport(ctx, cmd, lvDevicePath(baseImageLV(params.ImageName)), params.BindAddress, basePort)
	if err != nil {
		return ExportBaseResult{}, err
	}

	// 2. The image directory tarred to a file, served file-backed over NBD on the next
	//    port. Stage the tar ONLY when nothing serves it yet — re-tarring under a live
	//    qemu-nbd would corrupt in-flight reads. -C so paths inside are relative to the
	//    image dir (the target extracts straight into its own image directory).
	tarPath := baseMetaTarPath(params.ImageName)
	metaServing, err := nbdPortServing(ctx, cmd, metaPort)
	if err != nil {
		return ExportBaseResult{}, err
	}
	if !metaServing {
		if _, err := cmd.Run(ctx, "sudo tar -cf {} -C {} .", tarPath, imageDirectory(params.ImageName)); err != nil {
			return ExportBaseResult{}, err
		}
	}
	metaSizeOutput, err := cmd.Run(ctx, "sudo stat -c %s {}", tarPath)
	if err != nil {
		return ExportBaseResult{}, err
	}
	metaSize, err := strconv.ParseInt(strings.TrimSpace(metaSizeOutput), 10, 64)
	if err != nil {
		return ExportBaseResult{}, err
	}
	metaPID, err := ensureNBDExport(ctx, cmd, tarPath, params.BindAddress, metaPort)
	if err != nil {
		return ExportBaseResult{}, err
	}

	baseSize, err := lvSizeBytes(ctx, cmd, baseImageLV(params.ImageName))
	if err != nil {
		return ExportBaseResult{}, err
	}
	return ExportBaseResult{
		NBDPort: basePort, NBDPID: basePID, BaseSizeBytes: baseSize,
		MetaPort: metaPort, MetaPID: metaPID, MetaSizeBytes: metaSize,
	}, nil
}

// baseMetaTarPath is the staged image-dir tarball, keyed by image name so concurrent
// base ships of different images never clash. Transient — cleanup removes it.
func baseMetaTarPath(image string) string {
	return runDirectory + "/migrate-base-meta-" + image + ".tar"
}

// ReceiveBaseParams is the target side of the ship: the image to receive, the base LV
// size (the source's blockdev size), the source's address, and which phase to run.
// The source ports and nbd client slots are DERIVED (base = NBDPort+2 / slot base+2,
// tar = NBDPort+3 / slot base+3).
type ReceiveBaseParams struct {
	ImageName  string
	DiskGB     int
	SourceHost string
	Phase      string
}

// ReceiveBase pulls a local base image from the source over NBD onto this target, so
// it gains the base rootfs LV + image directory it needs to migrate — and boot — a VM
// on that image. It uses dm-clone (not a plain dd) so the ship's progress is
// observable through the same PollHydration percent as a VM disk; the base is
// read-only, so it hydrates to 100% then collapses to a plain LV. Two phases, driven
// per tick by the controller (PollHydration runs between them, keyed on the base
// clone name):
//
//   - prepare:  nbd-client to the source base + meta exports; a writable thin LV; the
//     dm-clone; extract the image-dir tar. Idempotent — skips any existing artifact.
//   - finalize: guard 100%, collapse the dm-clone to the plain LV (a plain `remove`,
//     not the transparent VM-disk collapse — the base is not booted from), flip it
//     read-only, disconnect the nbd clients.
//
// A present, read-only base LV means a prior finalize completed — a no-op on either
// phase. Ports scripts/migration-receive-base.py.
func ReceiveBase(ctx context.Context, cmd commands, uuid string, params ReceiveBaseParams) error {
	port, err := NBDPort(uuid)
	if err != nil {
		return err
	}
	baseSlot, err := NBDBaseSlot(uuid)
	if err != nil {
		return err
	}
	ship := baseShip{
		image:        params.ImageName,
		diskGB:       params.DiskGB,
		sourceHost:   params.SourceHost,
		basePort:     port + 2,
		metaPort:     port + 3,
		baseNBDSlot:  baseSlot + 2,
		metaNBDSlot:  baseSlot + 3,
		cloneName:    baseCloneName(params.ImageName),
		cloneMetaLV:  cloneMetaLV(baseShipKey(params.ImageName)),
		baseImageLV:  baseImageLV(params.ImageName),
		imageDirPath: imageDirectory(params.ImageName),
	}

	// Already fully received? A present, read-only base LV means a prior finalize
	// completed — nothing to do on either phase.
	if lvExists(ctx, cmd, ship.baseImageLV) {
		readOnly, err := lvIsReadOnly(ctx, cmd, ship.baseImageLV)
		if err != nil {
			return err
		}
		if readOnly {
			return nil
		}
	}

	switch params.Phase {
	case "prepare":
		return ship.prepare(ctx, cmd)
	case "finalize":
		return ship.finalize(ctx, cmd)
	default:
		return errUnknownPhase(params.Phase)
	}
}

// baseShip bundles the derived names and ports a base ship addresses, so prepare and
// finalize share them without re-deriving.
type baseShip struct {
	image                     string
	diskGB                    int
	sourceHost                string
	basePort, metaPort        int
	baseNBDSlot, metaNBDSlot  int
	cloneName, cloneMetaLV    string
	baseImageLV, imageDirPath string
}

func (ship baseShip) prepare(ctx context.Context, cmd commands) error {
	for _, module := range []string{"nbd", "dm_clone"} {
		if !cmd.OK(ctx, "sudo modprobe {}", module) {
			return errKernelModuleMissing(module)
		}
	}
	if !cmd.OK(ctx, "which nbd-client") {
		return errNBDClientMissing
	}
	tooFull, err := poolPastThreshold(ctx, cmd, poolHydrationThreshold)
	if err != nil {
		return err
	}
	if tooFull {
		return errThinPoolTooFull
	}

	// 1. A writable thin LV the base hydrates INTO (collapsed + flipped read-only at
	//    finalize). Named atlas-image-<name> so the collapse lands the base exactly
	//    where provision-vm / clone-target expect it.
	if err := createThin(ctx, cmd, ship.baseImageLV, ship.diskGB); err != nil {
		return err
	}
	// 2. Repair a wedged stack (dead source client) before rebuilding — same self-repair
	//    the VM-disk clone does.
	dropCloneIfSourceDead(ctx, cmd, ship.cloneName, ship.baseNBDSlot)
	// 3. nbd client to the source base export, then the dm-clone, size-verified.
	baseSize, err := lvSizeBytes(ctx, cmd, ship.baseImageLV)
	if err != nil {
		return err
	}
	baseNBD, err := ensureNBDClient(ctx, cmd, ship.sourceHost, ship.basePort, ship.baseNBDSlot, baseSize)
	if err != nil {
		return err
	}
	if err := ensureDMClone(ctx, cmd, ship.cloneName, ship.cloneMetaLV, ship.baseImageLV, baseNBD); err != nil {
		return err
	}
	// 4. The image directory (kernel + rootfs sentinel), small and instant — done here,
	//    not gated on hydration.
	return ship.receiveImageDir(ctx, cmd)
}

// receiveImageDir extracts the source's image-dir tar (served on the meta port) into
// this host's image directory. Idempotent: skips when the directory is already
// populated (a prior tick extracted it).
func (ship baseShip) receiveImageDir(ctx context.Context, cmd commands) error {
	if cmd.OK(ctx, "test -d {}", ship.imageDirPath) {
		if listing, _ := cmd.RunUnchecked(ctx, "ls -A {}", ship.imageDirPath); strings.TrimSpace(listing) != "" {
			return nil
		}
	}
	metaNBD, err := ensureNBDClient(ctx, cmd, ship.sourceHost, ship.metaPort, ship.metaNBDSlot, 0)
	if err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo install -d -m 0700 {}", ship.imageDirPath); err != nil {
		return err
	}
	// The tar was written with `-C <image_dir> .`, so paths are relative — extract
	// straight in. A shell reads the file-backed NBD export of the tar (gotcha #9).
	_, err = cmd.Shell(ctx, "sudo tar -xf {} -C {}", metaNBD, ship.imageDirPath)
	return err
}

func (ship baseShip) finalize(ctx context.Context, cmd commands) error {
	if cmd.OK(ctx, "sudo dmsetup info {}", ship.cloneName) {
		// Guard 100% before collapsing — a partial collapse leaves holes reading through
		// a torn-down NBD (same rule as the VM-disk collapse).
		status, err := cmd.Run(ctx, "sudo dmsetup status {}", ship.cloneName)
		if err != nil {
			return err
		}
		if !fullyHydrated(status) {
			return errBaseNotHydrated(ship.image, status)
		}
		// A plain remove (not the transparent collapse): the base is read-only and is
		// never booted from through the clone, so removing the mapping is safe.
		if _, err := cmd.Run(ctx, "sudo dmsetup remove {}", ship.cloneName); err != nil {
			return err
		}
		if err := removeLV(ctx, cmd, ship.cloneMetaLV); err != nil {
			return err
		}
	}
	// The base is never written after this — flip it read-only, like any synced or
	// snapshot-promoted base image.
	if lvExists(ctx, cmd, ship.baseImageLV) {
		readOnly, err := lvIsReadOnly(ctx, cmd, ship.baseImageLV)
		if err != nil {
			return err
		}
		if !readOnly {
			if _, err := cmd.Run(ctx, "sudo lvchange --permission r {}", lvReference(ship.baseImageLV)); err != nil {
				return err
			}
		}
	}
	// Disconnect the nbd clients (best-effort; -d is idempotent on a free slot).
	cmd.RunUnchecked(ctx, "sudo nbd-client -d /dev/nbd{}", ship.baseNBDSlot)
	cmd.RunUnchecked(ctx, "sudo nbd-client -d /dev/nbd{}", ship.metaNBDSlot)
	return nil
}
