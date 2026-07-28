// Package model holds the records Boat persists — the shapes that live in bbolt
// and survive a restart.
//
// These are deliberately not the wire types. internal/wire is generated from
// api/openapi.yaml and describes what Atlas sees; this package describes what
// the host remembers. The two overlap today and will not later: the journal
// grows checkpoints, fence epochs and adoption provenance that the API has no
// business exposing. Mapping between them is a handful of lines in internal/api
// and is worth paying to keep the store free to evolve.
package model

import "time"

// VirtualMachineStatus is Boat's observed status for one VM.
//
// It is derived from the host — the systemd unit's state and the on-disk
// markers — never from whether a command succeeded. That inversion is the point
// of the split: Atlas used to set status from Task success, which made the
// controller's picture a record of its own intentions rather than of the host.
type VirtualMachineStatus string

const (
	// StatusRunning means the unit is active: Firecracker is up with the guest in it.
	StatusRunning VirtualMachineStatus = "Running"
	// StatusStopped means the unit is inactive and the VM is not parked.
	StatusStopped VirtualMachineStatus = "Stopped"
	// StatusSleeping means the sleeping marker is present: the VM was parked to free
	// RAM and an inbound SYN wakes it. Distinct from Stopped, which stays stopped.
	StatusSleeping VirtualMachineStatus = "Sleeping"
	// StatusPaused means the guest is frozen through the Firecracker API while its
	// unit stays active. The unit alone cannot tell this from Running, so it is
	// read from the Firecracker that is answering on the VM's API socket and from
	// nowhere else.
	StatusPaused VirtualMachineStatus = "Paused"
	// StatusFailed means the unit reached its failed state — a distinct fact from
	// Stopped, which is a VM that was asked to stop and did.
	StatusFailed VirtualMachineStatus = "Failed"
	// StatusUnknown means the host could not be read — not that the VM is dead.
	// Boat reports what it saw; it never guesses on the host's behalf.
	StatusUnknown VirtualMachineStatus = "Unknown"
)

// VirtualMachine is one VM as this host observes it.
//
// Boat knows a VM only as a UUID plus mechanics. Nothing here says who owns it,
// what runs inside it, or what it costs — those are Atlas's questions and
// answering them here would re-entangle the two systems.
type VirtualMachine struct {
	UUID              string               `json:"uuid"`
	ObservedStatus    VirtualMachineStatus `json:"observed_status"`
	ObservedAt        time.Time            `json:"observed_at"`
	UnitActiveState   string               `json:"unit_active_state"`
	UnitSubState      string               `json:"unit_sub_state"`
	HasMemorySnapshot bool                 `json:"has_memory_snapshot"`
	Sleeping          bool                 `json:"sleeping"`
	// FirecrackerPID is the process that answered on this VM's API socket, or 0
	// when nothing did or when its holder could not be named. A diagnostic — it is
	// what lets an operator strace the right process — and nothing decides
	// anything from it: the socket answering is the liveness claim, and a pid on
	// its own would prove nothing (see internal/fcattach).
	FirecrackerPID int `json:"firecracker_pid"`
}

// OperationStatus is where one operation stands in its journal record.
type OperationStatus string

const (
	// OperationRunning means the operation was claimed and has not yet finished.
	// A record left in this state across a restart is what the reconciler resumes.
	OperationRunning OperationStatus = "Running"
	// OperationSuccess means the operation completed and its result is final.
	OperationSuccess OperationStatus = "Success"
	// OperationFailure means the operation failed; Error carries why.
	OperationFailure OperationStatus = "Failure"
)

// Operation is one entry in the append-only op journal.
//
// Identifier is the Atlas Task name, which is what makes a retry a replay: the
// same identifier posted twice returns this record rather than running the verb
// again. The journal is crash-recovery truth; the Task row remains the
// operator-facing audit record, and the two share this identifier.
type Operation struct {
	Identifier         string          `json:"operation_id"`
	Verb               string          `json:"verb"`
	VirtualMachineUUID string          `json:"uuid"`
	Status             OperationStatus `json:"status"`
	// Incarnation is which run of this daemon claimed the operation, stamped by
	// the transaction that claimed it. It is how work a crash abandoned is told
	// apart from work that is merely slow: a process that has ended cannot still
	// be running an operation claimed under the number it was handed when it
	// opened the store. See internal/journal's Unfinished.
	Incarnation int64     `json:"incarnation"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	ExitCode    int       `json:"exit_code"`
	Output      string    `json:"output"`
	Error       string    `json:"error"`
}

// Finished reports whether this operation reached a terminal state.
func (operation Operation) Finished() bool {
	return operation.Status == OperationSuccess || operation.Status == OperationFailure
}

// Matches reports whether a replayed request names the same work this record
// already covers. An identifier reused for different work is a caller bug and
// must be refused, not silently answered with someone else's result.
func (operation Operation) Matches(verb string, virtualMachineUuid string) bool {
	return operation.Verb == verb && operation.VirtualMachineUUID == virtualMachineUuid
}

// Quarantine is an artifact set the host holds that Boat could not read as a
// coherent VM — a crash part-way through a terminate, an LV with no unit, a
// unit with no disk.
//
// It is reported and never ingested as truth. The alternative — guessing which
// half of a torn-down VM is real — is how a controller ends up booting a VM
// whose disk it already released.
type Quarantine struct {
	UUID     string    `json:"uuid"`
	Reason   string    `json:"reason"`
	Evidence []string  `json:"evidence"`
	SeenAt   time.Time `json:"seen_at"`
}
