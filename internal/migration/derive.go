// Package migration ports Atlas's cross-host VM migration saga (spec/33 §8) to
// host-side Boat operations. Atlas stays the saga orchestrator — it drives the
// phases in a star topology and never lets the two hosts talk on the control plane
// — while each phase becomes an RPC to the named host's Boat. See
// llm/wo-4-migration-map.md for the full phase table and the fencing model.
//
// This file is the part both hosts must agree on with no shared state: every
// per-VM resource a migration needs — the source's qemu-nbd port, the target's
// block of nbd client slots, the keep-address tunnel's device, its socat port and
// its route table — is a PURE FUNCTION OF THE UUID. That is what lets a teardown or
// a lost-task re-entry name the same devices from the UUID alone. The formulas and
// constants are a byte-for-byte port of atlas migration.py:nbd_port/nbd_base_slot
// and networking.py:derive_vm_tunnel*, and they are a contract: a drift here
// silently latches a second migration onto the first's live nbd device — the exact
// bug (wrong size → dm-clone "Invalid argument") a real double-migration hit on
// 2026-07-02, which is why the slots are derived and never allocated.
package migration

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	nbdPortBase = 10000
	nbdPortSpan = 5000
	// A migration's TARGET needs 4 contiguous nbd client slots: base+0 root,
	// base+1 data, base+2 base-image ship, base+3 image-dir tar. Hosts ship 16 nbd
	// devices (nbds_max=16), so (uuid % 4) * 4 fans four concurrent target
	// migrations across /dev/nbd0-15 with no overlap.
	nbdSlotsPerMigration          = 4
	maxConcurrentTargetMigrations = 4 // 4 * 4 = 16 = nbds_max

	tunnelPortBase  = 15000 // a window that does not overlap the nbd ports, so a
	tunnelPortSpan  = 5000  // VM can run its NBD export and forward tunnel at once
	tunnelTableBase = 20000
	tunnelTableSpan = 40000
)

// NBDPort is the SOURCE host's qemu-nbd port for a VM's root disk. The data disk
// listens on NBDPort+1, the base-image ship on NBDPort+2 and the image-dir tar on
// NBDPort+3 — the four-port block the source exports for one migration.
func NBDPort(uuid string) (int, error) {
	first16, _, _, err := hexWindows(uuid)
	if err != nil {
		return 0, err
	}
	return nbdPortBase + int(first16)%nbdPortSpan, nil
}

// NBDBaseSlot is the first of the VM's 4 contiguous nbd CLIENT slots on the TARGET
// (base+0 root, +1 data, +2 base-image, +3 image-dir tar). Named, never allocated,
// so clone/cutover/base-ship all address the same /dev/nbdN with no allocator.
func NBDBaseSlot(uuid string) (int, error) {
	_, next16, _, err := hexWindows(uuid)
	if err != nil {
		return 0, err
	}
	return int(next16) % maxConcurrentTargetMigrations * nbdSlotsPerMigration, nil
}

// TunnelDevice is the keep-address forward tunnel's tun name: mig6-<first 8 hex>,
// 13 chars and IFNAMSIZ-safe, deliberately distinct from the atlas-/wg- device
// families so the three never collide. One device per migrated VM, left up while
// the /128 is forwarded.
func TunnelDevice(uuid string) (string, error) {
	hex, err := normalizeHex(uuid)
	if err != nil {
		return "", err
	}
	return "mig6-" + hex[:8], nil
}

// TunnelPort is the per-VM localhost TCP port for the tunnel's socat carrier, in a
// window disjoint from the nbd ports (see the constants) so a VM's export and its
// forward tunnel never collide.
func TunnelPort(uuid string) (int, error) {
	first16, _, _, err := hexWindows(uuid)
	if err != nil {
		return 0, err
	}
	return tunnelPortBase + int(first16)%tunnelPortSpan, nil
}

// TunnelTable is the per-VM route-table id holding the single `default dev
// <tunnel>` return route an `ip -6 rule from <vmv6>` selects. Derived so the
// install (target-receive) and the teardown (collapse) name the same table with no
// stored state.
func TunnelTable(uuid string) (int, error) {
	_, _, first32, err := hexWindows(uuid)
	if err != nil {
		return 0, err
	}
	return tunnelTableBase + int(first32%tunnelTableSpan), nil
}

// hexWindows returns the three UUID hex slices the formulas read — hex[:4] and
// hex[4:8] as 16-bit values and hex[:8] as a 32-bit value — matching Python's
// int(uuid.UUID(v).hex[a:b], 16) exactly. normalizeHex has already proved the
// string is 32 hex digits, so the parses below cannot fail.
func hexWindows(uuid string) (uint16, uint16, uint32, error) {
	hex, err := normalizeHex(uuid)
	if err != nil {
		return 0, 0, 0, err
	}
	first16, _ := strconv.ParseUint(hex[0:4], 16, 32)
	next16, _ := strconv.ParseUint(hex[4:8], 16, 32)
	first32, _ := strconv.ParseUint(hex[0:8], 16, 64)
	return uint16(first16), uint16(next16), uint32(first32), nil
}

// normalizeHex reduces a UUID to its 32 lowercase hex digits, tolerating the
// dashed canonical form the way Python's uuid.UUID does. Anything that is not 32
// hex digits once the dashes are removed is refused — a derivation off a malformed
// UUID would name a device some other VM could own.
func normalizeHex(uuid string) (string, error) {
	var builder strings.Builder
	builder.Grow(32)
	for _, r := range uuid {
		switch {
		case r == '-':
			continue
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'F':
			builder.WriteRune(r + ('a' - 'A'))
		default:
			return "", fmt.Errorf("migration: %q is not a hex UUID", uuid)
		}
	}
	hex := builder.String()
	if len(hex) != 32 {
		return "", fmt.Errorf("migration: %q is not a 128-bit UUID", uuid)
	}
	return hex, nil
}
