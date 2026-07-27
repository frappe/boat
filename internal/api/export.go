package api

import (
	"context"
	"io"

	"github.com/frappe/boat/internal/version"
	"github.com/frappe/boat/internal/wire"
)

// GetExport is Boat re-asserting fact, the mirror image of Atlas re-asserting
// intent with a PUT. Run back to back the two resynchronize a host from any
// state, which is why this supersedes the polling sweep Atlas used to run.
func (server *Server) GetExport(ctx context.Context, request wire.GetExportRequestObject) (wire.GetExportResponseObject, error) {
	// The snapshot carries the epoch it was taken at because the store read both
	// in one transaction. Reading the epoch separately here would let a write land
	// between the two and hand the caller a CAS token for a state nobody saw.
	export, err := server.state.Snapshot()
	if err != nil {
		return internalFault("This host's observed state could not be snapshotted.", err), nil
	}
	// The trace of a read is discarded: there is no operation record for it to
	// belong to, and an export is not work the journal owes an entry for.
	facts, err := server.hostFacts(ctx, server.newRunner(io.Discard))
	if err != nil {
		return internalFault("This host's facts could not be read.", err), nil
	}
	export.Host = facts
	// The running binary's version is this process's own fact, so it comes from
	// the same place /health and /host answer from. Three endpoints disagreeing
	// about which Boat is running is how a stuck upgrade hides.
	export.Host.BoatVersion = version.Version
	return wire.GetExport200JSONResponse(exportToWire(export)), nil
}
