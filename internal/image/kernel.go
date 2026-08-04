package image

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// downloadKernel fetches the packed, zstd-compressed bzImage the Ubuntu cloud
// image ships (`vmlinuz`) and decompresses the embedded ELF `vmlinux`
// Firecracker needs. We verify the DOWNLOAD against KernelSHA256 (the digest of
// the packed artifact, from upstream SHA256SUMS); the extracted kernel is a
// derived artifact, so verifying the download is the integrity gate. Idempotent:
// a kernel already in place is left untouched (the probe runs as root — the image
// dir is 0700 root-owned).
func downloadKernel(ctx context.Context, cmd commands, params SyncImageParams, imageDirectory string) error {
	kernelPath := imageDirectory + "/" + params.KernelFilename
	if cmd.OK(ctx, "sudo test -f {}", kernelPath) {
		return nil // "Kernel already present. Skipping."
	}

	packedPath := kernelPath + ".vmlinuz"
	if _, err := cmd.Run(ctx, "sudo rm -f {} {}", packedPath+".part", packedPath); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo curl -fsSL --output {} {}", packedPath+".part", params.KernelURL); err != nil {
		return err
	}
	if _, err := cmd.Input(ctx, params.KernelSHA256+"  "+packedPath+".part", "sudo sha256sum -c -"); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo mv {} {}", packedPath+".part", packedPath); err != nil {
		return err
	}

	// Decompress the embedded vmlinux. The Ubuntu kernel is a PE/EFI bzImage whose
	// payload is a zstd frame followed by a 4-byte size trailer, so plain
	// `unzstd`/`zstd -d` reject it ("unsupported format" — trailing bytes after the
	// frame). `zstd -dc -f` decompresses the valid frame and ignores the trailer.
	// We can't use the kernel.org extract-vmlinux helper: it verifies with
	// `readelf`, absent on a stock Firecracker host (it silently yields a 0-byte
	// file). So: locate the zstd magic (28 b5 2f fd), decompress from there with
	// `-f`, and confirm the ELF magic (7f 45 4c 46). `xxd | grep -bo` gives a
	// hex-nibble offset (byte = /2); `tail -c +N` is 1-indexed (+1).
	hexOffset, err := cmd.Shell(
		ctx, "xxd -p {} | tr -d '\\n' | grep -bo '28b52ffd' | head -1 | cut -d: -f1", packedPath,
	)
	if err != nil {
		return err
	}
	hexOffset = strings.TrimSpace(hexOffset)
	if hexOffset == "" {
		return fmt.Errorf("no zstd magic in kernel image %s", packedPath)
	}
	nibbleOffset, err := strconv.Atoi(hexOffset)
	if err != nil {
		return fmt.Errorf("unreadable zstd magic offset %q in kernel image %s: %w", hexOffset, packedPath, err)
	}
	byteOffset := nibbleOffset / 2

	if _, err := cmd.Shell(ctx, "tail -c +{} {} | zstd -dc -f > {}", byteOffset+1, packedPath, kernelPath+".part"); err != nil {
		return err
	}
	magic, err := cmd.Shell(ctx, "head -c 4 {} | xxd -p", kernelPath+".part")
	if err != nil {
		return err
	}
	if strings.TrimSpace(magic) != "7f454c46" {
		if _, err := cmd.Run(ctx, "sudo rm -f {}", kernelPath+".part"); err != nil {
			return err
		}
		return errors.New("decompressed kernel is not ELF")
	}
	if _, err := cmd.Run(ctx, "sudo mv {} {}", kernelPath+".part", kernelPath); err != nil {
		return err
	}
	if _, err := cmd.Run(ctx, "sudo rm -f {}", packedPath); err != nil {
		return err
	}
	return nil
}
