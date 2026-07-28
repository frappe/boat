package reconcile

import "github.com/frappe/boat/internal/model"

// step is the one thing a pass does to close the gap it found. One step per
// pass, never a sequence: the next pass re-reads the host, so a step that turned
// out not to be enough is followed by another and a step that was wrong is not
// compounded by three more taken on stale facts.
type step int

const (
	stepNone step = iota
	stepStart
	stepStop
	stepWake
)

// String names the step, so an assertion about a plan fails as a sentence
// instead of as two integers.
func (step step) String() string {
	switch step {
	case stepStart:
		return "start"
	case stepStop:
		return "stop"
	case stepWake:
		return "wake"
	default:
		return "nothing"
	}
}

// plan decides what this pass does. It is a pure function of the three inputs
// that may decide it — what Atlas wants, what the host was just observed to be,
// and why this pass was asked for — so the rules below are readable in one place
// and testable without a host, a store or a goroutine.
//
// The rules, and the failure each one exists to prevent:
//
//   - **desired_power = Stopped outranks a wake.** A VM an operator stopped is
//     not resurrected by a stranger's SYN. This is written as a branch taken
//     before the trigger is ever consulted, because it is a rule and not an
//     emergent property: the alternative is a reconciler that happens to do the
//     right thing today and starts resurrecting stopped VMs the moment someone
//     adds a second reason to request a pass. The operator's stop must be the
//     kind of stop that stays. The whole system leans on this one branch — the
//     wake trap turns an unauthenticated inbound packet into a requested pass and
//     has no opinion of its own — so the rule is also expressed as scope below:
//     the Stopped half is not given the trigger at all.
//
//   - **Sleeping is a resting state of a Running desire, not a deviation from
//     it.** A VM parked by sleep-on-idle has desired_power = Running — that is
//     the whole point, it is expected back — so a sweep that treated Sleeping as
//     "not Running yet" would wake every sleeping VM within the interval and
//     sleep-on-idle would free no RAM at all. Only a pass asked for by name
//     resumes it, which is what the wake trap does when a SYN arrives, and it
//     resumes through Wake rather than Start.
//
//   - **A state the host could not be read into is not acted on.** Unknown means
//     "I could not see", never "it is dead", and a unit caught mid-transition is
//     the ordinary way to see it. Starting a VM because a `systemctl show` was
//     ambiguous is how two copies of one guest end up sharing a disk.
//
//   - **Paused is not Stopped.** A guest frozen through the Firecracker API has
//     an active unit; starting it does nothing and stopping it works normally, so
//     a Running desire leaves it alone (the resume verb owns that) and a Stopped
//     desire takes the unit down.
func plan(desired model.DesiredVirtualMachine, observed model.VirtualMachine, why trigger) step {
	switch desired.DesiredPower {
	case model.PowerStopped:
		return stopStep(observed)
	case model.PowerRunning:
		return startStep(observed, why)
	default:
		// A desired record with no power stated says nothing about power, and Boat
		// does not guess on the control plane's behalf.
		return stepNone
	}
}

// stopStep is the Stopped half. Nothing here consults the trigger, and it is
// spelled as a missing parameter rather than as a discipline: no reason for
// asking can turn a Stopped desire into a start, so the reason is not in scope
// to be read. Adding it back is the change that would resurrect stopped VMs.
func stopStep(observed model.VirtualMachine) step {
	switch observed.ObservedStatus {
	case model.StatusRunning, model.StatusPaused:
		return stepStop
	case model.StatusSleeping:
		// A sleeping VM's unit is already inactive and its RAM is already given
		// back, so there is no power for this pass to converge. Unparking it — the
		// marker, the route, the nft rule — belongs to the sleep and wake verbs,
		// and stopping an inactive unit on every pass would be a spin that never
		// changes what the next pass sees.
		return stepNone
	default:
		return stepNone
	}
}

// startStep is the Running half, and the only half that is handed the trigger.
func startStep(observed model.VirtualMachine, why trigger) step {
	switch observed.ObservedStatus {
	case model.StatusStopped, model.StatusFailed:
		return stepStart
	case model.StatusSleeping:
		// Resumed only when someone asked for this VM by name, and resumed by Wake
		// rather than Start. A sleeping VM's unit carries
		// ConditionPathExists=! condition=<the sleeping marker>, so `systemctl start` skips
		// the unit, exits 0, and leaves the guest exactly as down as it found it —
		// the pass then fails on its own is-active check and backs off, forever.
		// Wake removes the marker first, which is the only order that works.
		if why == triggerRequest {
			return stepWake
		}
		return stepNone
	default:
		return stepNone
	}
}
