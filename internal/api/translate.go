package api

import (
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
