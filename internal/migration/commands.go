package migration

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
)

// commands is everything a migration phase does to the host, and the only seam
// the package has. Every phase renders its command sequence through this
// interface, so a recorder fake can assert the exact sequence with no qemu-nbd,
// no dmsetup and no root — the division park.go and vmdisk.go draw, kept here for
// the same reason: the command sequence a phase emits is the whole of what a host
// with none of those tools can check, and it is exactly what a differential test
// against the Python migration-*.py scripts compares.
//
// Outside tests there is one implementation, *run.Runner, and there is never a
// second. Probe carries the third answer (Yes/No/Unknown) for the one place a
// migration REPORTS a fact rather than guarding a mutation — dm-clone source
// liveness — where collapsing "could not look" into "dead" would trigger a
// destructive rebuild off a probe nobody could make (see clone.go).
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	Shell(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
	Probe(ctx context.Context, template string, parameters ...any) (run.Answer, error)
}

var _ commands = (*run.Runner)(nil)

const (
	// volumeGroup + poolName mirror vmdisk/bootstrap: VM disks are thin LVs carved
	// from atlas/pool0. thinPool is the qualified reference lvcreate --thinpool takes,
	// matching internal/vmdisk so both packages address the pool identically.
	volumeGroup = "atlas"
	poolName    = "pool0"
	thinPool    = volumeGroup + "/" + poolName

	// runDirectory holds a migration's transient artifacts — the qemu-nbd pidfiles
	// and the staged image-dir tar. Mirrors scripts/migration-*.py RUN_DIRECTORY.
	runDirectory = "/var/lib/atlas/run"

	// regionSectors is the dm-clone region — 16 MiB (32768 * 512). It is the unit
	// hydration is counted in; a drift from clone-target's value would make the
	// hydrated/total pair poll-hydration parses meaningless. Mirrors REGION_SECTORS.
	regionSectors = 32768

	// poolTooFullPercent gates a snapshot (source) and a hydration destination
	// (target): a thin snapshot is free up front but every later CoW write allocates,
	// so an almost-full pool courts a stall mid-migration. 90 is the source's
	// too-full-to-snapshot line (lvm.py FULL_THRESHOLD); the target refuses a
	// hydration onto a pool already past 80 (clone-target/receive-base guard).
	poolFullThreshold      = 90.0
	poolHydrationThreshold = 80.0
)

// The LV, snapshot, dm-clone and clone-metadata names a migration addresses. All
// derived from the UUID (or an image name) and never stored — a teardown or a
// lost-task re-entry reconstructs the same device from the UUID alone, which is
// what lets every phase be idempotent with no shared state. Byte-for-byte ports of
// the templates in scripts/lib/atlas/lvm.py and the migration-*.py scripts.
func vmDiskLV(uuid string) string     { return "atlas-vm-" + uuid }
func dataDiskLV(uuid string) string   { return "atlas-data-" + uuid }
func rootSnapLV(uuid string) string   { return "atlas-snap-" + uuid + "-migrate" }
func dataSnapLV(uuid string) string   { return "atlas-datasnap-" + uuid + "-migrate" }
func baseImageLV(image string) string { return "atlas-image-" + image }

// vmCloneName is the dm-clone device for a VM disk (key = uuid, or uuid+"-data"
// for the data disk); baseCloneName is the base-image ship's clone (keyed by image
// name, decoupled from any VM). cloneMetaLV is the small zeroed metadata LV either
// clone needs (key = uuid, uuid+"-data", or "base-"+image).
func vmCloneName(key string) string     { return "atlas-vm-" + key + "-clone" }
func baseCloneName(image string) string { return "atlas-base-" + image + "-clone" }
func cloneMetaLV(key string) string     { return "atlas-clonemeta-" + key }

// baseShipKey is the clone-metadata key for a base-image ship: "base-<image>", so
// the ship's clone and metadata never collide with a per-VM disk clone running for
// the same migration.
func baseShipKey(image string) string { return "base-" + image }

// lvReference is atlas/<name>, the form the LVM CLI tools take. lvDevicePath is
// /dev/atlas/<name>, the block node. Callers never hand-build either.
func lvReference(name string) string  { return volumeGroup + "/" + name }
func lvDevicePath(name string) string { return "/dev/" + volumeGroup + "/" + name }

// nbdPidFile is the qemu-nbd pidfile for a port, so the export's idempotency probe
// and cleanup's kill both name the same file with no stored state.
func nbdPidFile(port int) string {
	return fmt.Sprintf("%s/migrate-nbd-%d.pid", runDirectory, port)
}

// imageDirectory is where one image's on-disk artifacts (kernel + rootfs sentinel)
// live — the second thing a base-image ship carries besides the rootfs LV.
func imageDirectory(image string) string { return paths.ImageDirectory(image) }

// uplinkRequired is the IPv6 default-route device a proxy-NDP re-assert answers on,
// and it must exist: a source that stops answering NDP for a forwarded /128 black-
// holes ALL public ingress to it (proven in the field — the whole reason the
// re-assert is unconditional). Run, not OK, and errors when there is no uplink, so
// the missing route surfaces rather than an empty device splicing into the command.
func uplinkRequired(ctx context.Context, cmd commands) (string, error) {
	output, err := cmd.Run(ctx, "ip -j -6 route show default")
	if err != nil {
		return "", err
	}
	device, err := firstRouteDevice(output)
	if err != nil {
		return "", fmt.Errorf("no IPv6 default route to answer proxy-NDP on: %w", err)
	}
	return device, nil
}

// uplinkTolerant is uplinkRequired's teardown twin: a forward-down must proceed
// even on a host that has since lost its default route, so "" simply skips the
// proxy-NDP delete rather than failing the teardown.
func uplinkTolerant(ctx context.Context, cmd commands) string {
	output, err := cmd.RunUnchecked(ctx, "ip -j -6 route show default")
	if err != nil {
		return ""
	}
	device, err := firstRouteDevice(output)
	if err != nil {
		return ""
	}
	return device
}

// firstRouteDevice reads the dev of the first route in `ip -j route show`'s JSON —
// the same parse internal/netapply uses, kept local so the package needs no host to
// test it. An empty array means no default route.
func firstRouteDevice(output string) (string, error) {
	var routes []struct {
		Device string `json:"dev"`
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil {
		return "", err
	}
	if len(routes) == 0 || routes[0].Device == "" {
		return "", fmt.Errorf("no default route in %q", output)
	}
	return routes[0].Device, nil
}
