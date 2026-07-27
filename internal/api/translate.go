package api

import (
	"fmt"
	"time"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/wire"
)

// The store's records and the wire's documents are deliberately separate types:
// the journal will grow checkpoints, fence epochs and adoption provenance that
// the API has no business exposing. This file is the whole price of that
// separation, and it is small enough to be worth paying.

func virtualMachinesToWire(records []model.VirtualMachine) []wire.VirtualMachine {
	documents := make([]wire.VirtualMachine, 0, len(records))
	for _, record := range records {
		documents = append(documents, virtualMachineToWire(record))
	}
	return documents
}

func virtualMachineToWire(record model.VirtualMachine) wire.VirtualMachine {
	return wire.VirtualMachine{
		Uuid:              record.UUID,
		ObservedStatus:    wire.VirtualMachineStatus(record.ObservedStatus),
		ObservedAt:        record.ObservedAt,
		UnitActiveState:   &record.UnitActiveState,
		UnitSubState:      &record.UnitSubState,
		HasMemorySnapshot: &record.HasMemorySnapshot,
		Sleeping:          &record.Sleeping,
	}
}

// desiredFromWire reads one assertion of desired state.
//
// The path names the VM. A body naming a different one is refused rather than
// reconciled: a PUT that quietly wrote to the UUID in the body would let one
// typo re-fence a VM the caller never meant to touch.
func desiredFromWire(uuid string, document wire.DesiredVirtualMachine) (model.DesiredVirtualMachine, error) {
	if document.Uuid != "" && document.Uuid != uuid {
		return model.DesiredVirtualMachine{}, fmt.Errorf(
			"this request names %s in its path and %s in its body, so it is not clear which VM it asserts", uuid, document.Uuid)
	}
	power := model.DesiredPower(document.DesiredPower)
	if power != model.PowerRunning && power != model.PowerStopped {
		return model.DesiredVirtualMachine{}, fmt.Errorf(
			"desired_power must be %q or %q, and this request carried %q", model.PowerRunning, model.PowerStopped, document.DesiredPower)
	}
	return model.DesiredVirtualMachine{
		UUID:               uuid,
		BootEpoch:          int64(document.BootEpoch),
		DesiredPower:       power,
		VCPUs:              optional(document.Vcpus),
		CPUMaxCores:        optional(document.CpuMaxCores),
		CPUMode:            optional(document.CpuMode),
		MemoryMegabytes:    optional(document.MemoryMegabytes),
		DiskGigabytes:      optional(document.DiskGigabytes),
		DataDiskGigabytes:  optional(document.DataDiskGigabytes),
		SleepOnIdle:        optional(document.SleepOnIdle),
		IdleTimeoutSeconds: optional(document.IdleTimeoutSeconds),
		IPv6Address:        optional(document.Ipv6Address),
		PrivateAddress:     optional(document.PrivateAddress),
		MACAddress:         optional(document.MacAddress),
		TapDevice:          optional(document.TapDevice),
		// AssertedAt is the host's note of when it heard this, and the wire
		// carries no such field on purpose: a timestamp the asserting side
		// controls is a timestamp that can be backdated.
		AssertedAt: time.Now().UTC(),
	}, nil
}

func desiredToWire(record model.DesiredVirtualMachine) wire.DesiredVirtualMachine {
	return wire.DesiredVirtualMachine{
		Uuid:               record.UUID,
		BootEpoch:          record.BootEpoch,
		DesiredPower:       wire.DesiredPower(record.DesiredPower),
		Vcpus:              &record.VCPUs,
		CpuMaxCores:        &record.CPUMaxCores,
		CpuMode:            &record.CPUMode,
		MemoryMegabytes:    &record.MemoryMegabytes,
		DiskGigabytes:      &record.DiskGigabytes,
		DataDiskGigabytes:  &record.DataDiskGigabytes,
		SleepOnIdle:        &record.SleepOnIdle,
		IdleTimeoutSeconds: &record.IdleTimeoutSeconds,
		Ipv6Address:        &record.IPv6Address,
		PrivateAddress:     &record.PrivateAddress,
		MacAddress:         &record.MACAddress,
		TapDevice:          &record.TapDevice,
	}
}

// exportToWire renders the whole-host document.
//
// Units and logical volumes are carried only when the snapshot holds them. An
// empty array would say "this host has no logical volumes", which is a claim
// nothing in WO-1 has looked at the host closely enough to make.
func exportToWire(export model.Export) wire.Export {
	document := wire.Export{
		ObservedEpoch:   export.ObservedEpoch,
		TakenAt:         export.TakenAt,
		Host:            hostFactsToWire(export.Host),
		VirtualMachines: virtualMachinesToWire(export.VirtualMachines),
	}
	if export.Units != nil {
		units := unitsToWire(export.Units)
		document.Units = &units
	}
	if export.LogicalVolumes != nil {
		volumes := logicalVolumesToWire(export.LogicalVolumes)
		document.LogicalVolumes = &volumes
	}
	if len(export.Quarantined) > 0 {
		// Only when there is some. An empty array and an absent key both mean
		// "nothing quarantined", and the absent one keeps a healthy host's export
		// free of a field that only ever matters when it is non-empty.
		quarantine := quarantineToWire(export.Quarantined)
		document.Quarantine = &quarantine
	}
	if export.FenceEpochs != nil {
		epochs := fenceEpochsToWire(export.FenceEpochs)
		document.FenceEpochs = &epochs
	}
	return document
}

// hostFactsToWire reports what this host measured, and says nothing about what
// it could not.
//
// Absent, never zero, for an unmeasured fact. Atlas reads a present-but-zero
// number as a measurement: `api/server_capacity.py` treats a zero pool total as
// falsy, which makes the axis total None, which means UNLIMITED to placement.
// So a host whose thin pool cannot be read would keep accepting VMs forever if
// this sent 0 — strictly worse than the loud failure that behaviour replaced.
// The same applies to an empty Firecracker version, which would overwrite the
// one the Server row already knew.
func hostFactsToWire(facts model.HostFacts) wire.HostFacts {
	document := wire.HostFacts{
		Hostname:             facts.Hostname,
		BoatVersion:          facts.BoatVersion,
		KernelVersion:        &facts.KernelVersion,
		VcpusTotal:           &facts.VCPUsTotal,
		MemoryMegabytesTotal: &facts.MemoryMegabytesTotal,
		MemoryMegabytesFree:  &facts.MemoryMegabytesFree,
		HostSignature:        &facts.HostSignature,
	}
	document.PoolDiskGigabytesTotal = facts.PoolDiskGigabytesTotal
	document.PoolUsedPercent = facts.PoolUsedPercent
	if facts.FirecrackerVersion != "" {
		version := facts.FirecrackerVersion
		document.FirecrackerVersion = &version
	}
	return document
}

func unitsToWire(units []model.UnitLiveness) []wire.UnitLiveness {
	documents := make([]wire.UnitLiveness, 0, len(units))
	for _, unit := range units {
		documents = append(documents, wire.UnitLiveness{Name: unit.Name, ActiveState: unit.ActiveState, SubState: unit.SubState})
	}
	return documents
}

func logicalVolumesToWire(volumes []model.LogicalVolume) []wire.LogicalVolume {
	documents := make([]wire.LogicalVolume, 0, len(volumes))
	for _, volume := range volumes {
		documents = append(documents, wire.LogicalVolume{
			Name:      volume.Name,
			SizeBytes: &volume.SizeBytes,
			Pool:      &volume.Pool,
			Origin:    &volume.Origin,
		})
	}
	return documents
}

// fenceEpochsToWire narrows int64 to the int the IDL asks for. An epoch is a
// counter Atlas bumps once per migration, so nothing this side of the heat death
// of the datacentre overflows it — see the report on api/openapi.yaml.
func fenceEpochsToWire(epochs map[string]int64) map[string]int64 {
	document := make(map[string]int64, len(epochs))
	for uuid, epoch := range epochs {
		document[uuid] = epoch
	}
	return document
}

// optional reads a field the IDL made optional. An absent number is zero and an
// absent flag is false, which is what every one of these fields means when Atlas
// leaves it out.
func optional[Value any](field *Value) Value {
	if field == nil {
		var absent Value
		return absent
	}
	return *field
}

// operationToWire omits what a still-running operation does not have yet: an
// end time and an exit code on a Running record would be zero values pretending
// to be facts.
func operationToWire(operation model.Operation) wire.Operation {
	document := wire.Operation{
		OperationId: operation.Identifier,
		Verb:        operation.Verb,
		Uuid:        operation.VirtualMachineUUID,
		Status:      wire.OperationStatus(operation.Status),
		StartedAt:   operation.StartedAt,
	}
	if operation.Finished() {
		document.EndedAt = &operation.EndedAt
		document.ExitCode = &operation.ExitCode
	}
	if operation.Output != "" {
		document.Output = &operation.Output
	}
	if operation.Error != "" {
		document.Error = &operation.Error
	}
	return document
}

// quarantineToWire reports each artifact set that could not be read as a VM.
//
// Identifier, not UUID: a stranded namespace or address keeps only its own
// name, and inventing a UUID for it would record a guess as a fact.
func quarantineToWire(records []model.Quarantine) []wire.Quarantine {
	document := make([]wire.Quarantine, 0, len(records))
	for _, record := range records {
		entry := wire.Quarantine{Identifier: record.UUID, Reason: record.Reason}
		if len(record.Evidence) > 0 {
			evidence := record.Evidence
			entry.Evidence = &evidence
		}
		if !record.SeenAt.IsZero() {
			seenAt := record.SeenAt
			entry.SeenAt = &seenAt
		}
		document = append(document, entry)
	}
	return document
}
