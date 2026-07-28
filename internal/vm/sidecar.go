package vm

import (
	"context"
	"fmt"
	"strconv"

	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/sidecar"
)

// firecrackerUIDKey is the network.env key carrying the per-VM POSIX uid the
// jailer de-privileges Firecracker to. provision writes it from the same
// derivation that builds the jail, so it is the host's own record of the number.
const firecrackerUIDKey = "ATLAS_FC_UID"

// FirecrackerUID reads a VM's own uid out of its network.env.
//
// The verbs that need it — sleep, which chowns the snapshot directory to the
// jailed Firecracker that writes into it, and rebuild, which chowns the new
// jail node back to it — do NOT take it from their caller. Atlas derives the
// uid from the UUID and so could send it, but a caller sending its own copy can
// send a stale one, and the failures are silent in both places: a snapshot
// directory owned by the wrong uid produces a snapshot that is never written,
// and a jail node owned by the wrong uid is a guest that cannot open its own
// disk. The sidecar is what the jailer was actually built from. Same argument
// park.ParkVirtualMachine makes for the address, for the same reason.
//
// Read through sudo rather than opened in process: the VM tree is 0700 owned by
// that very uid, so an in-process read would report a missing file.
//
// Every failure is loud. There is no default that is safe here — 0 is root.
func (manager *Manager) FirecrackerUID(
	ctx context.Context, runner *run.Runner, uuid string,
) (int, error) {
	files := manager.filesFor(uuid)
	text, err := manager.commandsFor(runner).Run(ctx, "sudo cat {}", files.networkEnvironment)
	if err != nil {
		return 0, err
	}
	value := sidecar.Value(text, firecrackerUIDKey)
	if value == "" {
		return 0, fmt.Errorf("%s names no %s; re-provision the VM", files.networkEnvironment, firecrackerUIDKey)
	}
	uid, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s in %s: %w", firecrackerUIDKey, files.networkEnvironment, err)
	}
	return uid, nil
}
