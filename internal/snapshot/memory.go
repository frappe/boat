package snapshot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/paths"
)

const (
	// The memory file is the size of the guest's RAM. Require that much plus a
	// margin to be free, so a capture can never wedge the host filesystem against
	// full — the host that runs out of space mid-snapshot loses far more than the
	// one that cold-boots next time. Same margin as vm/sleep.go.
	freeSpaceMarginBytes = 256 * 1024 * 1024
	bytesPerMebibyte     = 1024 * 1024

	// The jq expression that reads the guest's RAM size out of firecracker.json.
	guestMemoryQuery = `."machine-config".mem_size_mib`
)

// checkMemoryFileSpace drops any previous snapshot directory (reclaiming its
// space, and making sure a stale marker cannot survive a failure below), then
// reports the reason there is not enough room for a fresh RAM-sized memory file,
// or "" to proceed. A returned error is different from a reason: the reason says
// "capture is not possible", the error says the host could not be read at all.
//
// The rm -rf comes first in both snapshot-stop and warm, so it is shared here.
func checkMemoryFileSpace(ctx context.Context, cmd commands, vm paths.VirtualMachine) (string, error) {
	if _, err := cmd.Run(ctx, "sudo rm -rf {}", vm.MemorySnapshotDirectory()); err != nil {
		return "", err
	}
	output, err := cmd.Run(ctx, "sudo jq -r {} {}", guestMemoryQuery, vm.FirecrackerConfig())
	if err != nil {
		return "", err
	}
	memoryMebibytes, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return "", fmt.Errorf("machine-config mem_size_mib in %s: %w", vm.FirecrackerConfig(), err)
	}
	output, err = cmd.Run(ctx, "df --output=avail -B1 {}", paths.AtlasRoot)
	if err != nil {
		return "", err
	}
	available, err := availableBytes(output)
	if err != nil {
		return "", err
	}
	if available < memoryMebibytes*bytesPerMebibyte+freeSpaceMarginBytes {
		return fmt.Sprintf(
			"not enough free space for a %d MiB memory file (%d B available)", memoryMebibytes, available,
		), nil
	}
	return "", nil
}

// availableBytes reads `df --output=avail -B1`, whose first line is the AVAIL
// header and whose second is the number.
func availableBytes(output string) (int64, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("df reported no free space line: %q", output)
	}
	return strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
}

// installAndOwnMemoryDirectory creates the in-jail snapshot directory 0700 and
// hands it to the per-VM uid, because the JAILED Firecracker is what writes the
// vmstate and memory files into it.
func installAndOwnMemoryDirectory(
	ctx context.Context, cmd commands, vm paths.VirtualMachine, firecrackerUID int,
) error {
	if err := cmd.InstallDirectory(ctx, vm.MemorySnapshotDirectory(), memorySnapshotDirectoryMode); err != nil {
		return err
	}
	owner := fmt.Sprintf("%d:%d", firecrackerUID, firecrackerUID)
	_, err := cmd.Run(ctx, "sudo chown {} {}", owner, vm.MemorySnapshotDirectory())
	return err
}

// writeAndVerifyMemoryPair asks Firecracker to write the vmstate/memory pair into
// the jail, then verifies both files landed non-empty. The verification is
// belt-and-suspenders: everything a later start (or a warm restore) does with the
// pair asserts a COMPLETE pair, so a half-written one that passed would be loaded
// and would fail. The caller must have PAUSED the guest first.
func writeAndVerifyMemoryPair(ctx context.Context, cmd commands, vm paths.VirtualMachine) error {
	if err := cmd.FirecrackerAPI(
		ctx, vm.APISocketDirectory(), vm.APISocketName(), "PUT", "/snapshot/create", memorySnapshotBody,
	); err != nil {
		return err
	}
	for _, file := range []string{vm.MemorySnapshotVMState(), vm.MemorySnapshotMemory()} {
		if !cmd.OK(ctx, "sudo test -s {}", file) {
			return fmt.Errorf("snapshot file missing or empty: %s", file)
		}
	}
	return nil
}

// memoryFileSize is the on-disk size of a captured memory file, the RAM the
// capture actually froze. A file that cannot be stat-ed is reported rather than
// guessed at — a zero would read as a snapshot that captured nothing.
func memoryFileSize(ctx context.Context, cmd commands, path string) (int64, error) {
	output, err := cmd.Run(ctx, "sudo stat -c %s {}", path)
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("memory file size: %w", err)
	}
	return size, nil
}
