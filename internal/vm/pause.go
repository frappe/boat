package vm

import (
	"context"
	"fmt"

	"github.com/frappe/boat/internal/run"
)

// The two guest states the Firecracker API takes on PATCH /vm. Spelled with the
// Python's exact spacing: these strings are compared byte for byte against
// pause-vm.py and resume-vm.py by the differential gate, and the sudoers line
// that authorizes the call pins the body literally.
const (
	pauseStateBody  = `{"state": "Paused"}`
	resumeStateBody = `{"state": "Resumed"}`
)

// Pause freezes the guest's vCPUs.
//
// This goes through the Firecracker API and NOT through systemd, and the
// difference is the whole point: the unit stays active, Firecracker stays
// resident, and the guest's RAM stays allocated. Pause frees CPU, not memory —
// the verb that frees memory is Sleep. A caller that wanted the unit down
// wanted Stop.
//
// Idempotent: Firecracker answers 2xx for a pause of an already-paused microVM,
// so a retried pause is not an error.
func (manager *Manager) Pause(ctx context.Context, runner *run.Runner, uuid string) error {
	return manager.setGuestState(ctx, runner, uuid, pauseStateBody)
}

// Resume unfreezes the guest's vCPUs — the inverse of Pause, and idempotent for
// the same reason: Firecracker ignores a resume of a microVM that is already
// running and still answers 2xx.
func (manager *Manager) Resume(ctx context.Context, runner *run.Runner, uuid string) error {
	return manager.setGuestState(ctx, runner, uuid, resumeStateBody)
}

// setGuestState is the shared body of Pause and Resume: the same call, to the
// same path, with a different state.
//
// The socket check first, because the failure it turns into a sentence is the
// common one. No socket means no running Firecracker, and PATCHing a socket
// that is not there fails with a connection error that says nothing about the
// VM. Note the two different addressings of the same socket, which is not a
// redundancy: the existence test takes the ABSOLUTE path, because stat has no
// length limit, while the API call takes the directory plus the short relative
// name, because the absolute path is longer than AF_UNIX's 108-byte sun_path
// and connect(2) would refuse it outright.
func (manager *Manager) setGuestState(
	ctx context.Context, runner *run.Runner, uuid string, body string,
) error {
	commands := manager.commandsFor(runner)
	files := manager.filesFor(uuid)
	if !commands.OK(ctx, "sudo test -S {}", files.apiSocket) {
		return fmt.Errorf("API socket %s not present; is the VM running?", files.apiSocket)
	}
	return commands.FirecrackerAPI(
		ctx, files.apiSocketDirectory, files.apiSocketName, "PATCH", "/vm", body,
	)
}
