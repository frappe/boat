package reconcile

import (
	"bytes"
	"context"
	"fmt"

	"github.com/frappe/boat/internal/fence"
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/vm"
)

// converge is one forward-only pass over one VM: read what the host is, decide
// the one step that closes the gap to what Atlas wants, take it, and write down
// what the host became.
//
// It is safe to re-enter at any point, because it assumes nothing about how the
// previous pass ended. Every input is re-read from the store and the host, and
// the step it takes is idempotent on its own — `systemctl start` on a running
// unit and `systemctl stop` on a stopped one are both no-ops. A pass killed
// half-way leaves no state for the next one to unwind.
//
// The caller runs this inside Do, so it is already this VM's actor.
func (reconciler *Reconciler) converge(ctx context.Context, uuid string, why trigger) error {
	desired, found, err := reconciler.store.GetDesired(uuid)
	if err != nil {
		return fmt.Errorf("read the desired state of %s: %w", uuid, err)
	}
	if !found {
		// No assertion from Atlas is no authority to act. A VM whose artifacts are
		// on this disk may already be running on the host it migrated to, so the
		// reconciler waits to be told rather than converging what it finds.
		return nil
	}
	// A pass has no operation record to carry a command trace, so the trace is
	// kept only long enough to attach it to a failure: the steady state is a pass
	// that does nothing, and tracing that every interval would bury the one that
	// mattered.
	var trace bytes.Buffer
	if err := reconciler.drive(ctx, run.NewRunner(&trace), desired, why); err != nil {
		logger(uuid).Error("could not reconcile a virtual machine", "error", err, "trace", trace.String())
		return err
	}
	return nil
}

func (reconciler *Reconciler) drive(
	ctx context.Context, runner *run.Runner, desired model.DesiredVirtualMachine, why trigger,
) error {
	observed, err := reconciler.observe(ctx, runner, desired.UUID)
	if err != nil {
		return err
	}
	step := plan(desired, observed, why)
	if step == stepNone {
		return nil
	}
	if err := reconciler.apply(ctx, runner, desired, step); err != nil {
		return err
	}
	// Observed again after acting, because the record has to say what the host is
	// and not what the step asked for. That inversion is why Boat exists: a status
	// written from "the command succeeded" is a record of the controller's
	// intentions, and it is how a VM that died an hour ago still reads Running.
	_, err = reconciler.observe(ctx, runner, desired.UUID)
	return err
}

// observe reads the host and writes down what it saw. The write is part of the
// pass rather than an afterthought: an observation nobody recorded is one Atlas
// cannot read out of the export, and the export is how a partitioned control
// plane catches up.
func (reconciler *Reconciler) observe(
	ctx context.Context, runner *run.Runner, uuid string,
) (model.VirtualMachine, error) {
	observed, err := reconciler.machines.Observe(ctx, runner, uuid)
	if err != nil {
		return observed, fmt.Errorf("observe %s: %w", uuid, err)
	}
	if err := reconciler.store.PutVirtualMachine(observed); err != nil {
		return observed, fmt.Errorf("record what %s was observed to be: %w", uuid, err)
	}
	// The write bumped the observed epoch; now tell the watchers a change landed.
	// Announced only after it is written down, for the reason the post-verb path
	// is: a watcher told of a transition the store does not hold would read the
	// export next and see it undone. A reconciler with no publisher wired is a
	// no-op here, never a nil call.
	reconciler.observed(observed)
	return observed, nil
}

// apply takes the one step the plan chose.
func (reconciler *Reconciler) apply(
	ctx context.Context, runner *run.Runner, desired model.DesiredVirtualMachine, step step,
) error {
	switch step {
	case stepStart:
		return reconciler.start(ctx, runner, desired)
	case stepWake:
		return reconciler.wake(ctx, runner, desired)
	case stepStop:
		// The zero StopRequest is the cooperative stop: the guest is asked to power
		// itself off and its filesystems are synced. A reconciler has no reason to
		// discard a guest's writes — the callers that do (a migration cold-stop)
		// come through a verb that says so.
		return reconciler.machines.Stop(ctx, runner, desired.UUID, vm.StopRequest{})
	}
	return nil
}

// start refuses to boot a VM this host is not fenced for.
//
// The reconciler is gated exactly as the API's start verb is, and for the same
// reason: a Boat that lost its bbolt file and booted everything it found on disk
// is the most dangerous state in the system, because the VM whose artifacts are
// still here may already be running on the host it was migrated to. A background
// loop makes that worse than a verb does — nobody has to ask for it — so the
// fence is consulted on every pass rather than trusted from the last one.
func (reconciler *Reconciler) start(
	ctx context.Context, runner *run.Runner, desired model.DesiredVirtualMachine,
) error {
	if err := reconciler.allowedToBoot(desired); err != nil {
		return err
	}
	_, err := reconciler.machines.Start(ctx, runner, desired.UUID)
	return err
}

// wake resumes a sleeping VM, and is fenced exactly as a cold start is.
//
// The gate matters more here, not less. A pass that reaches this step was asked
// for by name, and the thing that asks by name is the wake trap — so the input
// is an unauthenticated inbound packet, and this fence is the last check between
// that packet and a guest booting on a host that may no longer own it. Waking is
// booting: the marker comes off and the unit comes up.
func (reconciler *Reconciler) wake(
	ctx context.Context, runner *run.Runner, desired model.DesiredVirtualMachine,
) error {
	if err := reconciler.allowedToBoot(desired); err != nil {
		return err
	}
	return reconciler.machines.Wake(ctx, runner, desired.UUID)
}

// allowedToBoot asks the fence, under the epoch Atlas asserted with the desired
// state this pass is acting on. A pass that started a VM under an epoch other
// than the one it read would be answering a question nobody asked.
func (reconciler *Reconciler) allowedToBoot(desired model.DesiredVirtualMachine) error {
	heldEpoch, held, err := reconciler.store.FenceEpoch(desired.UUID)
	if err != nil {
		return fmt.Errorf("read the fence epoch of %s: %w", desired.UUID, err)
	}
	if err := fence.Allow(heldEpoch, held, desired.BootEpoch); err != nil {
		return fmt.Errorf("refusing to boot %s: %w", desired.UUID, err)
	}
	return nil
}
