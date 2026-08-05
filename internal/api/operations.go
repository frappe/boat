package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/store"
	"github.com/frappe/boat/internal/vm"
	"github.com/frappe/boat/internal/wire"
)

// errUnknownVirtualMachine is a failure like any other as far as the journal is
// concerned: the operation was claimed, so it owes a terminal record even when
// the answer to the caller is 404.
var errUnknownVirtualMachine = errors.New("this host has no such virtual machine")

func (server *Server) StartVirtualMachine(ctx context.Context, request wire.StartVirtualMachineRequestObject) (wire.StartVirtualMachineResponseObject, error) {
	if request.Body == nil || request.Body.OperationId == "" {
		return missingOperationIdentifier(), nil
	}
	if failure := server.refuseUnfenced(request.Uuid); failure != nil {
		return failure, nil
	}
	start := func(ctx context.Context, runner *run.Runner) (model.OperationResult, error) {
		_, err := server.virtualMachines.Start(ctx, runner, request.Uuid)
		return nil, err
	}
	operation, failure := server.perform(ctx, request.Body.OperationId, verbStartVirtualMachine, request.Uuid, start)
	if failure != nil {
		return failure, nil
	}
	return wire.StartVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

func (server *Server) StopVirtualMachine(ctx context.Context, request wire.StopVirtualMachineRequestObject) (wire.StopVirtualMachineResponseObject, error) {
	if request.Body == nil || request.Body.OperationId == "" {
		return missingOperationIdentifier(), nil
	}
	stopRequest := stopRequestFrom(request.Body)
	stop := func(ctx context.Context, runner *run.Runner) (model.OperationResult, error) {
		return nil, server.virtualMachines.Stop(ctx, runner, request.Uuid, stopRequest)
	}
	operation, failure := server.perform(ctx, request.Body.OperationId, verbStopVirtualMachine, request.Uuid, stop)
	if failure != nil {
		return failure, nil
	}
	return wire.StopVirtualMachine200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// stopRequestFrom applies the IDL's defaults: a stop is cooperative unless the
// caller says otherwise, and an unset timeout leaves systemd's drain alone.
//
// This is the one place the polarity flips. The wire says `graceful` because
// that is what Atlas and the Python verb have always called it; vm.StopRequest
// says `Forced` so its zero value is the stop that does not lose writes. An
// absent field therefore means a cooperative stop on both sides, which is what
// the IDL's `default: true` promises.
func stopRequestFrom(body *wire.StopRequest) vm.StopRequest {
	request := vm.StopRequest{}
	if body.Graceful != nil {
		request.Forced = !*body.Graceful
	}
	if body.StopTimeoutSeconds != nil {
		request.TimeoutSeconds = *body.StopTimeoutSeconds
	}
	return request
}

// hostWork is one verb's mechanics as perform runs them: the work itself, and
// the typed result it produced.
//
// The result is in the signature rather than captured by the closure because
// perform is what writes the terminal record, and a value that reached the
// record any other way would be a second path out of a verb — the thing this
// package keeps closing. Eight of the nine verbs return nil: they report their
// trace and nothing else, and nil is how "nothing to report" stays
// distinguishable from "reported false".
type hostWork func(ctx context.Context, runner *run.Runner) (model.OperationResult, error)

// perform is the shared body of every verb: claim, take the VM's turn, run,
// record — in that order, and never any other.
//
// The claim comes first because the identifier is the Atlas Task name: a
// retried Task must return its first result rather than boot the VM a second
// time. Only a caller that wins the claim runs anything, and it runs it as the
// VM's actor. Claiming inside the turn instead would make a replay — a read of a
// record that already exists — queue behind whatever boot is in progress.
//
// Every verb in this package goes through here, which is what makes "a verb
// never touches the host directly" structural rather than a rule each new
// handler has to remember.
// The response is the CLAIM, not the outcome. A verb runs on after its request
// is answered and the caller reads the result from `GET /ops/{operation_id}`;
// see runInBackground for why that is the shape.
func (server *Server) perform(ctx context.Context, identifier, verb, uuid string, execute hostWork) (model.Operation, *errorResponse) {
	// Before the claim and before any host command: a malformed uuid must not
	// even reserve an operation identifier, let alone reach the runner. Every one
	// of the nine verbs funnels through here, which is what makes this the one
	// place the boundary check has to live for them.
	if failure := server.refuseMalformedUUID(uuid); failure != nil {
		return model.Operation{}, failure
	}
	// The quiesce gate, consulted before the claim so a raised gate leaves no
	// journal entry to answer for.
	//
	// The gate, its drain and the refusal below were all written for this call
	// site and none of them was called from it: server.go said "every mutating
	// verb consults it", POST /v1/quiesce drained a count that never counted, and
	// unavailableWhileQuiescing's own comment describes the answer perform hands
	// back. A self-update could therefore swap the binary under a running verb.
	if !server.admission.enter() {
		return model.Operation{}, unavailableWhileQuiescing()
	}
	admitted := true
	defer func() {
		if admitted {
			server.admission.leave()
		}
	}()
	operation, claimed, err := server.operations.ClaimOperation(identifier, verb, uuid)
	switch {
	case errors.Is(err, store.ErrOperationConflict):
		return operation, conflict("Operation " + identifier + " is already recorded against different work.")
	case err != nil:
		return operation, internalFault("The operation could not be claimed.", err)
	case !claimed:
		return operation, nil
	}
	// From here the work is the background's, and so is the gate's release.
	admitted = false
	finished := server.runInBackground(operation, uuid, execute)
	if respondAsync(ctx) {
		// The caller polls. The claim is the whole answer, and the operation it
		// names is the one `GET /ops/{operation_id}` will report on.
		return operation, nil
	}
	// The caller waits, so this waits for the same work rather than doing it a
	// second way: the operator's `boat vm start` blocks and is answered exactly
	// as it always was, including the 404 for a VM this host does not hold and
	// the 500 for a journal it could not write. One execution path, two answers.
	concluded := <-finished
	return concluded.operation, concluded.failure
}

// outcome is what a finished verb has to say: the record, and the answer a
// waiting caller gets.
type outcome struct {
	operation model.Operation
	failure   *errorResponse
}

// runInBackground runs a claimed verb and hands back a channel that closes when
// it is done. Every verb goes through here whether or not its caller waits, so
// the two answers differ in what they wait for and never in what they do.
//
// The work runs under the DAEMON's context, never the request's, and that is the
// whole reason hostWork takes a context instead of closing over the handler's: a
// request context is cancelled the moment its response is written, so work that
// outlives the response would be cancelled at birth.
//
// It is also why a client hanging up no longer abandons a verb. The operation
// belongs to the daemon from the claim onwards and the journal answers for it,
// which is what makes a lost connection survivable: Atlas re-reads
// `GET /ops/{operation_id}` and finds the record either in flight or finished,
// where before it had a Task it could never answer.
//
// The wait group is what shutdown drains, so a verb in flight still reaches a
// terminal record before the store closes.
func (server *Server) runInBackground(operation model.Operation, uuid string, execute hostWork) <-chan outcome {
	finished := make(chan outcome, 1)
	server.background.Add(1)
	go func() {
		defer server.background.Done()
		defer server.admission.leave()
		recorded, failure := server.asActor(server.backgroundContext, operation, uuid, execute)
		if failure != nil {
			slog.Error(
				"operation failed", "operation", operation.Identifier,
				"verb", operation.Verb, "uuid", uuid,
			)
		}
		finished <- outcome{operation: recorded, failure: failure}
	}()
	return finished
}

// asActor runs the claimed verb inside the reconciler, so that this verb, any
// other verb for the same UUID and any reconcile pass over it are one queue.
//
// The runner is built inside the turn too, because runClaimed builds it: a
// verb's trace then covers its own commands and not the tail of the operation
// it was waiting for.
func (server *Server) asActor(
	ctx context.Context, operation model.Operation, uuid string, execute hostWork,
) (model.Operation, *errorResponse) {
	recorded, failure := operation, (*errorResponse)(nil)
	err := server.reconciler.Do(ctx, uuid, func(ctx context.Context) error {
		recorded, failure = server.runClaimed(ctx, operation, uuid, execute)
		return nil
	})
	if err != nil {
		return server.abandoned(operation, uuid, err)
	}
	return recorded, failure
}

// abandoned records a verb that never got its turn — the client hung up, or the
// daemon is shutting down.
//
// The claim is already in the journal by then, so it owes a terminal record
// whatever happened to the caller: an operation left Running is one whose Atlas
// Task can never be answered, and the retry that would rescue it reads the same
// non-terminal record and is refused again.
//
// The out-of-process case — the daemon itself dying between the claim and the
// record — cannot be closed from here, and is closed by the reconciler's resume
// on the next start (internal/reconcile, conclude). The claim carries this run's
// incarnation for exactly that reason.
func (server *Server) abandoned(operation model.Operation, uuid string, cause error) (model.Operation, *errorResponse) {
	var trace bytes.Buffer
	recorded, failure := server.record(operation, &trace, nil, fmt.Errorf("waiting for a turn on %s: %w", uuid, cause))
	if failure == nil {
		failure = internalFault("This request was abandoned before it could run.", cause)
	}
	return recorded, failure
}

// runClaimed runs the verb behind a claim and journals the outcome. For a
// polling caller the record IS the answer, so nothing may return from here
// without one, or Atlas would poll an operation that never reaches a terminal
// state.
func (server *Server) runClaimed(ctx context.Context, operation model.Operation, uuid string, execute hostWork) (model.Operation, *errorResponse) {
	var trace bytes.Buffer
	runner := server.newRunner(&trace)
	// A VM this host does not hold is a failure like any other to the journal:
	// the operation was claimed, so it owes a terminal record even though the
	// answer to a waiting caller is 404. A polling caller reads that record and
	// sees the same refusal a moment later.
	if !server.virtualMachines.Exists(ctx, runner, uuid) {
		recorded, failure := server.record(operation, &trace, nil, errUnknownVirtualMachine)
		if failure == nil {
			failure = notFound("This host has no virtual machine " + uuid + ".")
		}
		return recorded, failure
	}
	result, verbError := execute(ctx, runner)
	if verbError == nil {
		server.observe(ctx, runner, uuid, &trace)
	}
	return server.record(operation, &trace, result, verbError)
}

// record writes the terminal journal entry. The trace buffer becomes Output, so
// the Task row Atlas shows carries the same `+ command` lines it always has, and
// the verb's typed result rides beside it — Atlas folds that back into the one
// `ATLAS_RESULT=` line an SSH script would have printed, so a caller parses one
// Task the same way whichever transport filled it.
//
// Only a SUCCEEDING verb's result is kept. A verb that failed may still have
// computed half of one, and recording it would hand a caller a value to act on
// out of an operation that did not finish.
func (server *Server) record(operation model.Operation, trace *bytes.Buffer, result model.OperationResult, verbError error) (model.Operation, *errorResponse) {
	operation.EndedAt = time.Now().UTC()
	operation.Output = trace.String()
	operation.Status = model.OperationSuccess
	operation.Result = result
	if verbError != nil {
		operation.Result = nil
		operation.Status = model.OperationFailure
		operation.Error = verbError.Error()
		operation.ExitCode = exitCodeOf(verbError)
	}
	if err := server.operations.CompleteOperation(operation); err != nil {
		return operation, internalFault("The operation could not be recorded.", err)
	}
	return operation, nil
}

// exitCodeOf mirrors the exit code the equivalent Task carried. A failure that
// never reached a command has no exit code of its own, so it reports 1.
func exitCodeOf(verbError error) int {
	var commandError *run.CommandError
	if errors.As(verbError, &commandError) {
		return commandError.ExitCode
	}
	return 1
}

// observe persists what the host says now, so Boat's store reflects the host
// rather than the request. A verb that succeeded stays succeeded even if the
// observation afterwards did not: the record says so instead of lying either
// way.
func (server *Server) observe(ctx context.Context, runner *run.Runner, uuid string, trace *bytes.Buffer) {
	record, err := server.virtualMachines.Observe(ctx, runner, uuid)
	if err == nil {
		err = server.operations.PutVirtualMachine(record)
	}
	if err != nil {
		fmt.Fprintf(trace, "# could not observe %s after the verb: %v\n", uuid, err)
		slog.Error("could not observe virtual machine after a verb", "uuid", uuid, "error", err)
		return
	}
	// Announced only once it is written down. A watcher told of a transition the
	// store does not hold would read the export next and see it undone.
	server.publishObserved(record)
}

// missingOperationIdentifier refuses work that could not be replayed. The IDL
// makes operation_id required and the generated server does not enforce it, so
// the boundary does: without it a retry would boot the VM twice.
func missingOperationIdentifier() *errorResponse {
	return badRequest("This request needs an operation_id, so a retry can be recognised as a replay.")
}
