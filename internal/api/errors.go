package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/frappe/boat/internal/wire"
)

// errorResponse is the one shape every refusal takes: a status code, a single
// plain sentence, and — where the caller has to branch — a stable token. It
// implements each generated response interface, so a handler refuses a request
// the same way whichever operation was asked for.
//
// The sentence is written for the caller. A raw Go error would hand Atlas a
// path, an argv or a bbolt offset it can do nothing with, so the detail stays
// on the host in the daemon's log and only the fact crosses the boundary.
type errorResponse struct {
	statusCode int
	message    string
	// reason is the machine-readable half, and it is set only where the prose
	// cannot carry the weight: two refusals that share a status code and call for
	// opposite behaviour from the caller. It is empty otherwise, and then the
	// field is absent on the wire — a token nobody branches on is a token that
	// drifts away from the sentence beside it without anyone noticing.
	reason wire.ErrorReason
}

func (response *errorResponse) write(writer http.ResponseWriter) error {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(response.statusCode)
	body := wire.Error{Error: response.message}
	if response.reason != "" {
		body.Reason = &response.reason
	}
	return json.NewEncoder(writer).Encode(body)
}

func (response *errorResponse) VisitGetHostResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitListVirtualMachinesResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitGetVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitGetOperationResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitStartVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitStopVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitPauseVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitResumeVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitSleepVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitWakeVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitRebuildVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitTerminateVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitResizeVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitReservedIpVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitPutVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitDeleteVirtualMachineResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitGetExportResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitGetUnitResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func (response *errorResponse) VisitActOnUnitResponse(writer http.ResponseWriter) error {
	return response.write(writer)
}

func notFound(message string) *errorResponse {
	return &errorResponse{statusCode: http.StatusNotFound, message: message}
}

// badRequest refuses a request the boundary understood and will not act on. The
// generated server checks shapes; the rules a shape cannot express — an
// operation with no identifier, a document naming two different VMs — are
// checked here, because the alternative is acting on them.
func badRequest(message string) *errorResponse {
	return &errorResponse{statusCode: http.StatusBadRequest, message: message}
}

// conflict answers an identifier reused for different work. Replay is only
// replay when it is the same operation; anything else is a caller bug and must
// be refused rather than answered with someone else's result.
func conflict(message string) *errorResponse {
	return &errorResponse{statusCode: http.StatusConflict, message: message}
}

// conflictBecause is conflict where the caller cannot act on the sentence alone.
//
// The PUT is where this became necessary: a fence regression and a stale
// observation are both 409, and one of them must never be retried while the
// other must be retried immediately against a fresh export. A caller told only
// "409" either retries the one that must not be — re-asserting a claim this host
// has seen superseded — or gives up on the one that would have succeeded.
func conflictBecause(reason wire.ErrorReason, message string) *errorResponse {
	return &errorResponse{statusCode: http.StatusConflict, message: message, reason: reason}
}

// internalFault states the fault in one sentence and keeps the cause on the
// host, where an operator reading journalctl can see it in full.
func internalFault(message string, cause error) *errorResponse {
	slog.Error(message, "error", cause)
	return &errorResponse{statusCode: http.StatusInternalServerError, message: message}
}

// writeError refuses a request before it reaches a typed handler — the paths
// where there is no generated response object to return, such as a failed
// authentication.
func writeError(writer http.ResponseWriter, statusCode int, message string) {
	response := errorResponse{statusCode: statusCode, message: message}
	if err := response.write(writer); err != nil {
		slog.Error("could not write an error response", "error", err)
	}
}
