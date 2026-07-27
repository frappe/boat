package vm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/run"
)

const (
	bytesPerGibibyte = 1024 * 1024 * 1024

	// The jq program that edits the two machine-config keys and leaves the rest
	// of firecracker.json — boot source, drives, network interfaces — untouched.
	machineConfigFilter = `."machine-config".vcpu_count = $vcpus | ."machine-config".mem_size_mib = $mem`

	// How the generated launcher writes one cgroup value: its own continued
	// line, four spaces of indent, the value shell-quoted. Rewriting to the same
	// shape means a re-provision after a resize produces an identical launcher
	// and nothing drifts.
	cgroupLinePrefix = "--cgroup "
	cgroupLineIndent = "    "
)

// ResizeRequest is the new shape of a stopped VM.
//
// Firecracker reads machine-config only at boot, so vCPU and memory take effect
// on the next start rather than now; the disk grows immediately.
type ResizeRequest struct {
	VCPUs    int
	MemoryMB int
	// DiskGB is the target root disk size. Grow-only, and zero means "leave the
	// disk alone" — a resize that changes only vCPU and memory.
	DiskGB int
	// CgroupArguments are the jailer's new cgroup values (memory.max=…,
	// cpu.max=…), derived by the caller from the new memory and vCPU counts.
	//
	// Passing them is not cosmetic. The guest RAM in firecracker.json and the
	// host cgroup memory.max in the launcher are two INDEPENDENT ceilings:
	// resizing only the first hands the guest RAM the second still caps below,
	// the guest allocates past memory.max, and the kernel OOM-kills Firecracker
	// on the next boot (CONSTRAINT_MEMCG, "Failed with result 'signal'"). Empty
	// leaves the launcher untouched, which is right only for a caller that is
	// not changing memory.
	CgroupArguments []string
	// DataDiskGB is the target size of the data disk. Zero means the VM has
	// none, or that this resize does not touch it.
	DataDiskGB int
	// DataDiskFormatted says the data disk carries a filesystem to grow with the
	// volume. The zero value grows the block device only, which is what a raw
	// attached disk wants and is the safe way round: a volume grown without its
	// filesystem is fixed by re-running this with the flag set, while resize2fs
	// pointed at a disk that is not ext4 is a conversation with a stranger's
	// bytes.
	DataDiskFormatted bool
}

// Resize sets a stopped VM's vCPU and memory for its next boot and grows its
// disks now.
//
// Idempotent from end to end: the config rewrite writes the same values, and
// lvextend is a no-op once the volume already meets the size.
func (manager *Manager) Resize(
	ctx context.Context, runner *run.Runner, uuid string, request ResizeRequest,
) error {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	rootVolume, dataVolume := rootDisk(uuid), dataDisk(uuid)
	growData, err := manager.resizePreflight(ctx, commands, files, rootVolume, dataVolume, request)
	if err != nil {
		return err
	}
	// The machine config, and possibly the disk, is about to change under any
	// staged memory snapshot; the saved vmstate would no longer match it. Drop
	// it so the next start cold-boots into the new shape.
	if _, err := commands.Run(ctx, "sudo rm -rf {}", files.memorySnapshotDirectory); err != nil {
		return err
	}
	if err := manager.writeMachineConfig(ctx, commands, files, request); err != nil {
		return err
	}
	if err := manager.rewriteLauncherCgroups(ctx, commands, files, request.CgroupArguments); err != nil {
		return err
	}
	rootVolume.grow(ctx, commands, request.DiskGB, true)
	if growData {
		dataVolume.grow(ctx, commands, request.DataDiskGB, request.DataDiskFormatted)
	}
	return nil
}

// resizePreflight refuses the whole resize before it has changed anything, and
// reports whether the data disk is there to grow.
//
// Everything that can be checked is checked here rather than as it comes up,
// because a resize that fails halfway has written a new machine-config for a
// disk it then declined to grow, and nothing reconciles that.
func (manager *Manager) resizePreflight(
	ctx context.Context, commands commands, files virtualMachineFiles,
	rootVolume volume, dataVolume volume, request ResizeRequest,
) (bool, error) {
	if !commands.OK(ctx, "sudo test -f {}", files.firecrackerConfig) {
		return false, fmt.Errorf(
			"firecracker config %s missing; provision the VM first", files.firecrackerConfig,
		)
	}
	if !rootVolume.exists(ctx, commands) {
		return false, fmt.Errorf("disk volume %s missing; provision the VM first", rootVolume.name)
	}
	if err := refuseToShrink(ctx, commands, rootVolume, request.DiskGB); err != nil {
		return false, err
	}
	if request.DataDiskGB <= 0 || !dataVolume.exists(ctx, commands) {
		return false, nil
	}
	return true, refuseToShrink(ctx, commands, dataVolume, request.DataDiskGB)
}

// refuseToShrink declines a target smaller than the volume already is.
//
// A disk only ever grows. Shrinking the volume under a filesystem that has
// already written past the new boundary destroys whatever lives there, and no
// amount of care at this layer can tell which blocks those are. lvextend
// happens to refuse a shrink too, but its exit code is discarded — so without
// this the request would be silently ignored, and "silently ignored" is how a
// caller learns that shrinking works.
func refuseToShrink(ctx context.Context, commands commands, disk volume, gigabytes int) error {
	if gigabytes <= 0 {
		return nil
	}
	current, err := disk.sizeBytes(ctx, commands)
	if err != nil {
		return err
	}
	if int64(gigabytes)*bytesPerGibibyte < current {
		return fmt.Errorf(
			"refusing to shrink %s: it is %d B and %d GiB was asked for; a disk only grows",
			disk.name, current, gigabytes,
		)
	}
	return nil
}

// writeMachineConfig edits vcpu_count and mem_size_mib in place. jq touches
// only those two keys, so drives and network interfaces survive verbatim.
func (manager *Manager) writeMachineConfig(
	ctx context.Context, commands commands, files virtualMachineFiles, request ResizeRequest,
) error {
	updated, err := commands.Run(
		ctx, "sudo jq --argjson vcpus {} --argjson mem {} {} {}",
		request.VCPUs, request.MemoryMB, machineConfigFilter, files.firecrackerConfig,
	)
	if err != nil {
		return err
	}
	return replaceInPlace(ctx, commands, files.firecrackerConfig, updated, "0644")
}

// rewriteLauncherCgroups splices the new cgroup values into the generated
// jailer launcher, leaving every other line — the boot-args block, uid, netns,
// resource limits, chroot — byte for byte intact.
func (manager *Manager) rewriteLauncherCgroups(
	ctx context.Context, commands commands, files virtualMachineFiles, cgroupArguments []string,
) error {
	if len(cgroupArguments) == 0 {
		return nil
	}
	original, err := commands.Run(ctx, "sudo cat {}", files.jailerLaunch)
	if err != nil {
		return err
	}
	rewritten, err := spliceCgroupArguments(original, cgroupArguments)
	if err != nil {
		return fmt.Errorf("%s: %w", files.jailerLaunch, err)
	}
	return replaceInPlace(ctx, commands, files.jailerLaunch, rewritten, "0755")
}

// spliceCgroupArguments replaces the launcher's run of `--cgroup <value> \`
// lines with one rendered from the new values, in place and in order.
//
// Both refusals are deliberate. A launcher with no --cgroup lines is not one
// this rewrite understands — hand-edited, or generated by another version — and
// rewriting nothing would leave exactly the stale caps that cause the OOM-kill
// this splice exists to prevent. Non-contiguous lines mean a generator that
// interleaves other flags, and splicing across them would drop a line that is
// not a cgroup line at all. Failing loudly beats either.
func spliceCgroupArguments(launcher string, cgroupArguments []string) (string, error) {
	lines := strings.Split(strings.TrimSuffix(launcher, "\n"), "\n")
	var indices []int
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), cgroupLinePrefix) {
			indices = append(indices, index)
		}
	}
	if len(indices) == 0 {
		return "", errors.New("no --cgroup lines to rewrite; refusing to leave stale caps")
	}
	first, last := indices[0], indices[len(indices)-1]
	if last-first+1 != len(indices) {
		return "", errors.New("--cgroup lines are not contiguous; refusing to rewrite")
	}
	rendered := make([]string, 0, len(cgroupArguments))
	for _, value := range cgroupArguments {
		rendered = append(rendered, cgroupLineIndent+cgroupLinePrefix+run.Quote(value)+` \`)
	}
	spliced := append([]string{}, lines[:first]...)
	spliced = append(spliced, rendered...)
	spliced = append(spliced, lines[last+1:]...)
	return strings.Join(spliced, "\n") + "\n", nil
}

// replaceInPlace writes content beside path and moves it over.
//
// The ownership is copied from the original before the move, not set to root:
// the jailed Firecracker runs as the per-VM uid and reads its own config after
// the chroot, so a root-owned replacement is a VM that cannot start. The move
// is what makes the swap atomic — a reader sees the old file or the new one.
func replaceInPlace(
	ctx context.Context, commands commands, path string, content string, mode string,
) error {
	replacement := path + ".new"
	if err := commands.InstallFile(ctx, content, replacement, mode); err != nil {
		return err
	}
	if _, err := commands.Run(ctx, "sudo chown {} {}", "--reference="+path, replacement); err != nil {
		return err
	}
	_, err := commands.Run(ctx, "sudo mv {} {}", replacement, path)
	return err
}
