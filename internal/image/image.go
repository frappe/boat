// Package image bakes and promotes the base images every per-VM disk is a thin
// CoW snapshot of. It ports two Atlas per-host scripts, comments and all:
//
//   - SyncImage (scripts/sync-image.py): download a kernel + rootfs pair into
//     /var/lib/atlas/images/<name>/, decompress the packed vmlinux to a raw ELF
//     Firecracker can boot, and normalize the generic Ubuntu cloud squashfs into
//     a pristine Atlas ext4 (static-IPv6, no first-boot agent, baked guest
//     modules), then lay that ext4 down as the read-only base LV. Idempotent: a
//     kernel or ext4 already present is left as is.
//   - PromoteSnapshotImage (scripts/promote-snapshot-image.py): promote a baked
//     snapshot LV into a same-server base image, so new VMs provision from it via
//     the ordinary `image` field instead of cloning a one-off snapshot. The bytes
//     never leave the host — snapshot LV to base-image LV is a local dd.
//
// Everything here is a pure function over the `commands` seam: no curl, no LVM
// stack, no root, no host. The recorder tests assert the exact command sequence
// each verb emits, which is what a differential test against the Python compares.
package image

import (
	"context"

	"github.com/frappe/boat/internal/run"
)

// StagedGuestNetworkUnit is where the controller stages the guest
// atlas-network.service sidecar before a sync-image Task (SCRIPT_SIDECARS in
// script_uploads.py). This is ALWAYS the path for a controller-driven sync, so
// SyncImageParams.GuestNetworkUnit defaults to it — an operator hand-running the
// verb needn't pass one (they either point it at a real file or rely on this
// staged path being populated).
const StagedGuestNetworkUnit = "/tmp/atlas/atlas-network.service"

// commands is everything these verbs do to the host, and the only seam they
// have. It is a superset of internal/thinpool's own seam — Run, RunUnchecked and
// OK — so the thinpool import helpers take this interface value straight through.
// Input feeds a checksum manifest to `sha256sum -c -` without it reaching the
// process table; InstallFile/InstallDirectory are the install(1) create-with-mode
// helpers the shell heredocs relied on. Outside tests the one implementation is
// *run.Runner.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
	Input(ctx context.Context, stdin string, template string, parameters ...any) (string, error)
	Shell(ctx context.Context, template string, parameters ...any) (string, error)
	InstallFile(ctx context.Context, content string, destination string, mode string) error
	InstallDirectory(ctx context.Context, destination string, mode string) error
}

var _ commands = (*run.Runner)(nil)
