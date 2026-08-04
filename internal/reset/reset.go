// Package reset wipes a bootstrapped host back to its just-bootstrapped state.
// DESTRUCTIVE.
//
// It is the whole-host analogue of terminate: where terminate tears down one VM's
// on-host state, ResetServer sweeps EVERY VM, image, snapshot, forward tunnel and
// stray networking artifact off the host — leaving exactly what bootstrap produced
// (the atlas VG + empty pool0, the host hardening, the base nft scaffold). After it
// the host is immediately provision-ready; it does NOT need re-bootstrapping.
//
// It exists because a host's on-disk state can drift out of sync with the Frappe DB
// (the DB's VM/Image/Snapshot rows were lost while the host kept its LVs/units/
// jails). Rather than reconcile row by row, ResetServer wipes the host clean so the
// controller can start from an empty, known slate.
//
// Idempotent and best-effort: EVERY step tolerates already-gone state (the whole
// package runs through RunUnchecked), so a partial run can be re-run. There is no
// machine-readable result the caller must parse — a wipe just wipes — so the result
// only reports what was swept, for the operation record.
//
// What it KEEPS (the bootstrap floor): the atlas VG and the empty thin pool pool0;
// the host hardening, the mgmt firewall, and the base `inet atlas` table scaffold;
// the shared atlas-park0 dummy; the state directories themselves (emptied, not
// removed).
//
// What it REMOVES: every firecracker-vm@<uuid> unit + its directory; every
// atlas-mig6-* forward-tunnel unit; every atlas-vm-/atlas-data-/atlas-snap-/
// atlas-datasnap-/atlas-clonemeta- LV AND every atlas-image-* base-image LV (full
// scratch: no images kept); the images/ and snapshots/ contents; every atlas netns
// + host-side veth/tap/mig6 link; any bound nbd client + leftover dm-clone target;
// every per-VM nft forward rule + wake counter + NDP proxy entry.
//
// Ported from scripts/reset-server.py. The per-VM network teardown the Python ran
// as `<venv-python> vm-network-down.py <uuid>` is injected here as networkDown, so
// this package stays host-free in tests — the same seam internal/migration's
// CleanupSource uses.
package reset

import (
	"context"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
)

const (
	// volumeGroup + poolName are the bootstrap floor: pool0 in the atlas VG is kept,
	// every other atlas LV is removed. Mirrored from lvm.py:ThinPool.
	volumeGroup = "atlas"
	poolName    = "pool0"
)

// commands is everything this package does to the host, and the only seam it has.
// It is RunUnchecked-only on purpose: reset is best-effort end to end (the Python
// runs every step with check=False), so a missing unit, device or table is never an
// error, and there is nothing here whose failure should abort the wipe. Outside
// tests there is one implementation, *run.Runner.
type commands interface {
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
}

var _ commands = (*run.Runner)(nil)

// ResetParams is empty: a reset takes no options — it wipes the whole host.
type ResetParams struct{}

// ResetResult reports what was swept, for the operation record. The Python emits no
// ATLAS_RESULT line; these lists are the Go equivalent of its human progress prints.
type ResetResult struct {
	VirtualMachines   []string // per-VM units disabled + directories removed
	ForwardTunnels    []string // atlas-mig6-* units stopped
	NetworkNamespaces []string // atlas- netns deleted
	Links             []string // host-side veth/tap/mig6 links deleted
	NDPProxies        []string // proxy-NDP entries removed (by address)
	BoundNBD          []string // nbd clients disconnected
	DMCloneTargets    []string // dm-clone targets removed
	LogicalVolumes    []string // atlas LVs lvremoved (pool0 excluded)
}

// ResetServer wipes the host clean and reports what it swept. networkDown is boat's
// own vm-network-down for one VM, run per stopped VM before its directory is removed
// (so the sidecar the teardown reads still exists) — the durable, idempotent
// teardown hook, injected so this phase stays host-free in tests. A nil networkDown
// skips that step (the final directory sweep still removes the VM tree).
//
// ResetServer itself does not fail: every host poke is best-effort, so it always
// returns (result, nil). The error return is kept for the verb-signature contract
// the parent wires against.
func ResetServer(
	ctx context.Context, cmd commands, _ ResetParams, networkDown func(ctx context.Context, uuid string) error,
) (ResetResult, error) {
	result := ResetResult{}
	result.VirtualMachines = stopVirtualMachines(ctx, cmd, networkDown)
	result.ForwardTunnels = stopForwardTunnels(ctx, cmd)
	result.NetworkNamespaces, result.Links, result.NDPProxies = teardownNetworking(ctx, cmd)
	result.BoundNBD, result.DMCloneTargets = disconnectNBDAndDMClone(ctx, cmd)
	result.LogicalVolumes = removeLogicalVolumes(ctx, cmd)
	clearStateDirectories(ctx, cmd)
	return result, nil
}

// stopVirtualMachines disables+stops every per-VM firecracker unit and removes its
// directory. It runs the durable vm-network-down hook per VM FIRST (before the rm),
// so the netns/veth/nft/NDP state a stopped-but-not-terminated VM left behind is
// cleaned even when the unit's ExecStopPost never ran — the same fallback teardown
// terminate makes.
func stopVirtualMachines(ctx context.Context, cmd commands, networkDown func(context.Context, string) error) []string {
	uuids := listVMDirectories(ctx, cmd)
	for _, uuid := range uuids {
		unit := "firecracker-vm@" + uuid + ".service"
		cmd.RunUnchecked(ctx, "sudo systemctl disable --now {}", unit)
		if networkDown != nil {
			// Best-effort, like the Python's check=False: a teardown that could not
			// finish must not strand the rest of the wipe.
			_ = networkDown(ctx, uuid)
		}
		// The directory carries the jail tree; the LV its node points at is a separate
		// object removed later.
		cmd.RunUnchecked(ctx, "sudo rm -rf {}", paths.VirtualMachinesDirectory+"/"+uuid)
	}
	// Reset any units left failed so the names are reusable and a fresh VM's status is
	// clean.
	cmd.RunUnchecked(ctx, "sudo systemctl reset-failed 'firecracker-vm@*'")
	return uuids
}

// stopForwardTunnels stops every atlas-mig6-<port> transient forward-tunnel unit
// (the socat carriers). Stopping the unit tears down the socat process; the tun
// device it owned disappears with it.
func stopForwardTunnels(ctx context.Context, cmd commands) []string {
	units := listUnits(ctx, cmd, "atlas-mig6-*")
	for _, unit := range units {
		cmd.RunUnchecked(ctx, "sudo systemctl stop {}", unit)
	}
	cmd.RunUnchecked(ctx, "sudo systemctl reset-failed 'atlas-mig6-*'")
	return units
}

// teardownNetworking sweeps any VM networking artifact the per-VM teardown did not
// reach: stray atlas netns, host-side veth/tap/mig6 links, every NDP proxy entry,
// and the parked-VM SYN traps. Best-effort and idempotent. The masquerade rule and
// the base `inet atlas forward` chain scaffold are host-wide bootstrap state and are
// intentionally left in place — only per-VM rules are removed.
func teardownNetworking(ctx context.Context, cmd commands) (namespaces, links, proxies []string) {
	// atlas network namespaces (bootstrap creates none; every one here is a VM's).
	for _, netns := range listNetNS(ctx, cmd) {
		if isAtlasNamespace(netns) {
			cmd.RunUnchecked(ctx, "sudo ip netns del {}", netns)
			namespaces = append(namespaces, netns)
		}
	}
	// Host-side veth/tap/mig6 links whose peer/namespace is already gone.
	for _, link := range listAtlasLinks(ctx, cmd) {
		cmd.RunUnchecked(ctx, "sudo ip link del {}", link)
		links = append(links, link)
	}
	// Every NDP proxy entry (proxy-NDP is only ever installed per VM).
	for _, entry := range listNDPProxy(ctx, cmd) {
		cmd.RunUnchecked(ctx, "sudo ip -6 neigh del proxy {} dev {}", entry.address, entry.device)
		proxies = append(proxies, entry.address)
	}
	// Every parked-VM SYN trap: its forward rule + named counter (park). The shared
	// atlas-park0 dummy is bootstrap floor and is kept, like the nft table scaffold.
	sweepParkState(ctx, cmd)
	return namespaces, links, proxies
}

// disconnectNBDAndDMClone disconnects any bound nbd client and removes any lingering
// dm-clone target — the two device-mapper/nbd leftovers a mid-flight migration can
// strand on a host. Idempotent: a free device / absent target is a no-op.
func disconnectNBDAndDMClone(ctx context.Context, cmd commands) (nbd, dmTargets []string) {
	for _, device := range listBoundNBD(ctx, cmd) {
		cmd.RunUnchecked(ctx, "sudo nbd-client -d {}", device)
		nbd = append(nbd, device)
	}
	// dm-clone targets are named by the collapse key; sweep every live clone target.
	for _, name := range listDMTargets(ctx, cmd) {
		cmd.RunUnchecked(ctx, "sudo dmsetup remove {}", name)
		dmTargets = append(dmTargets, name)
	}
	return nbd, dmTargets
}

// removeLogicalVolumes removes every atlas-managed LV EXCEPT the thin pool itself —
// including the base-image LVs (a full scratch reset wants them gone), which the
// normal teardown refuses as protected shared state. pool0 and the VG survive so the
// host stays provision-ready without a re-bootstrap.
func removeLogicalVolumes(ctx context.Context, cmd commands) []string {
	var removed []string
	for _, name := range listAtlasLVs(ctx, cmd) {
		if name == poolName {
			continue
		}
		cmd.RunUnchecked(ctx, "sudo lvremove -f {}", volumeGroup+"/"+name)
		removed = append(removed, name)
	}
	return removed
}

// clearStateDirectories empties the per-VM, image and snapshot state directories but
// keeps the directories themselves (bootstrap created them 0755 root, and later
// provisions expect them to exist). The venv/bin the Python kept are not Boat's
// concern — Boat is one binary — so only these three trees are swept.
func clearStateDirectories(ctx context.Context, cmd commands) {
	for _, directory := range []string{
		paths.VirtualMachinesDirectory, paths.ImagesDirectory, paths.SnapshotsDirectory,
	} {
		// Remove the CONTENTS, not the directory. The literal `{}` is find's own
		// replacement token, passed as a parameter so it survives quoting unchanged.
		cmd.RunUnchecked(ctx, "sudo find {} -mindepth 1 -maxdepth 1 -exec rm -rf {} +", directory, "{}")
	}
}
