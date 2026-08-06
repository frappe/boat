package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/store"
	"github.com/frappe/boat/internal/wire"
)

// hostScopeKey is the turn a host-scoped verb takes — one that acts on the host
// rather than a single VM (sync-image, promote-snapshot-image, the s3 backups).
// They serialize against each other so two never overlap, and none queues behind
// a per-VM actor. A VM-scoped verb takes its VM's turn under the UUID instead, so
// snapshot-vm and a reconcile pass over the same VM can never run at once.
const hostScopeKey = "host"

// resultMarker is the one stdout line a host verb prints that is a value rather
// than trace — the ATLAS_RESULT= line scripts/lib/atlas/_task.py and
// cmd/boat/taskverb.go both write. A host verb runs in-process here, so the
// daemon reads that line back off the verb's stdout and folds it into the
// operation's typed `result`, exactly as Atlas folded it off an SSH script's
// stdout (§2.7). The constant is duplicated across the language boundary the
// same way scripts_catalog's verb names are — one string, three files agree.
const resultMarker = "ATLAS_RESULT="

// virtualMachineNameVariable is the UUID a VM-scoped host verb names — the VM it
// acts on and the turn it takes. Its presence is what tells a VM-scoped verb
// (snapshot-vm) from a host-scoped one (sync-image), so a verb never needs to
// declare its own scope.
const virtualMachineNameVariable = "VIRTUAL_MACHINE_NAME"

// HostVerbRunner runs the host verbs in-process. It is injected (cmd/boat) rather
// than built here: the verbs live beside the CLI that also runs them, and the
// daemon and the CLI must run ONE implementation of each verb, not two (§2.1). A
// Server built without a runner — a handler test, a tool — serves no host verb.
type HostVerbRunner interface {
	// Serves reports whether this host runs the named MUTATING verb, so the
	// boundary refuses an unknown one with 400 before it claims an identifier.
	Serves(verb string) bool
	// ServesRead reports whether the named verb is a READ-ONLY sweep, served over
	// /host-reads without a journal record. Disjoint from Serves: a mutating verb
	// is not a read and vice versa.
	ServesRead(verb string) bool
	// Run executes the verb, writing its trace to stderr and its one
	// ATLAS_RESULT= line (where it has a result) to stdout, and returns the exit
	// code the equivalent Task carried. It is called inside the verb's turn.
	Run(verb string, arguments []string, stdout, stderr io.Writer) int
}

// RunHostRead runs a read-only host verb and answers with its output, without a
// journal record or a turn. It is the transport for the per-minute sweeps
// (poll-vm-traffic, probe-woken-vms) Atlas drove through run_probe: they change
// nothing and run every tick, so journaling them would bury the audit log and
// serializing them would queue a read behind a boot. It also skips the quiesce
// gate, so a read keeps answering while a self-update drains the mutating verbs —
// the same rule §5 gives /export and /health.
func (server *Server) RunHostRead(ctx context.Context, request wire.RunHostReadRequestObject) (wire.RunHostReadResponseObject, error) {
	verb := request.Verb
	if server.hostVerbs == nil || !server.hostVerbs.ServesRead(verb) {
		return wire.RunHostRead400JSONResponse(
			wire.Error{Error: "This host serves no read-only host verb " + verb + "."},
		), nil
	}
	var variables *map[string]interface{}
	if request.Body != nil {
		variables = request.Body.Variables
	}
	arguments := hostVerbArguments(variables)
	var stdout, trace bytes.Buffer
	code := server.hostVerbs.Run(verb, arguments, &stdout, &trace)
	if code != 0 {
		return internalFault(
			"The read verb "+verb+" failed on the host.",
			&hostVerbError{verb: verb, code: code, detail: lastLine(trace.String())},
		), nil
	}
	// The verb's stdout carries its one ATLAS_RESULT= line, which is exactly what
	// run_probe read off the SSH channel and handed to parse_result — so the
	// controller reads a read the same way whichever transport carried it.
	return wire.RunHostRead200JSONResponse{Output: stdout.String()}, nil
}

// RunHostVerb is the transport for the host verbs — the operations that create
// disks, take snapshots, sync images and apply per-VM networking, each of which
// Atlas used to drive as `boat <verb>` over SSH. It is the host-verb twin of the
// lifecycle handlers: claim by op_id, take a turn, run, journal, so a retried
// Task returns its first result rather than running the verb twice.
func (server *Server) RunHostVerb(ctx context.Context, request wire.RunHostVerbRequestObject) (wire.RunHostVerbResponseObject, error) {
	if request.Body == nil || request.Body.OperationId == "" {
		return missingOperationIdentifier(), nil
	}
	verb := request.Verb
	if server.hostVerbs == nil || !server.hostVerbs.Serves(verb) {
		return badRequest("This host serves no host verb " + verb + "."), nil
	}
	// A VM-scoped verb names the VM it acts on; a host-scoped one names none. The
	// presence of the UUID variable is the whole of the distinction, and it
	// decides both the turn the verb takes and whether the host is re-observed
	// after it.
	uuid := hostVerbVariable(request.Body.Variables, virtualMachineNameVariable)
	scopeKey := hostScopeKey
	if uuid != "" {
		// A malformed UUID must not reach the runner or reserve an identifier —
		// the same boundary check the lifecycle verbs make of their path UUID.
		if failure := server.refuseMalformedUUID(uuid); failure != nil {
			return failure, nil
		}
		scopeKey = uuid
	}
	arguments := hostVerbArguments(request.Body.Variables)
	run := func(ctx context.Context, trace *bytes.Buffer) (model.OperationResult, error) {
		var stdout bytes.Buffer
		code := server.hostVerbs.Run(verb, arguments, &stdout, trace)
		// The verb's stdout carries any human lines and its one ATLAS_RESULT=
		// line. The human lines join the trace an operator reads; the result line
		// becomes the typed result a caller acts on.
		result, remainder := parseResultLine(stdout.String())
		trace.WriteString(remainder)
		if code != 0 {
			return nil, &hostVerbError{verb: verb, code: code, detail: lastLine(trace.String())}
		}
		return result, nil
	}
	operation, failure := server.performHost(ctx, request.Body.OperationId, verb, uuid, scopeKey, run)
	if failure != nil {
		return failure, nil
	}
	return wire.RunHostVerb200JSONResponse{
		OperationAcceptedJSONResponse: wire.OperationAcceptedJSONResponse(operationToWire(operation)),
	}, nil
}

// hostVerbRun is a host verb's mechanics as performHost runs them: it writes its
// trace into the buffer that becomes the operation's Output and returns the typed
// result a caller acts on. The result is in the signature rather than captured by
// the closure for the reason hostWork's is — perform writes the terminal record,
// and a value that reached it any other way would be a second path out of a verb.
type hostVerbRun func(ctx context.Context, trace *bytes.Buffer) (model.OperationResult, error)

// performHost is the host-verb twin of perform: claim, take a turn, run, record —
// same order, same journal, same claim-then-poll shape. It differs in exactly the
// two ways a host verb is not one VM's lifecycle:
//
//   - it takes no fence and no Exists gate. provision-vm CREATES the VM and
//     sync-image touches none, so a gate that required the VM to already exist
//     would refuse the very verbs that bring one into being — the same reason a
//     migration phase skips it (spec/33 §8).
//   - it serializes on scopeKey — a VM-scoped verb on its UUID, a host-scoped one
//     on a single host-wide key — rather than always a UUID.
//
// Everything else is perform's: the quiesce gate before the claim, the claim as
// the reply, and the work running under the daemon's context so it outlives the
// request that asked for it.
func (server *Server) performHost(
	ctx context.Context, identifier, verb, uuid, scopeKey string, run hostVerbRun,
) (model.Operation, *errorResponse) {
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
	admitted = false
	finished := make(chan outcome, 1)
	server.background.Add(1)
	go func() {
		defer server.background.Done()
		defer server.admission.leave()
		recorded, failure := server.asActorHost(server.backgroundContext, operation, scopeKey, uuid, run)
		finished <- outcome{operation: recorded, failure: failure}
	}()
	if respondAsync(ctx) {
		// The caller polls GET /ops/{operation_id}; the claim is the whole answer.
		return operation, nil
	}
	concluded := <-finished
	return concluded.operation, concluded.failure
}

// asActorHost runs a host verb inside its turn and journals the outcome — the
// host-verb twin of asActor + runClaimed, without the Exists gate and observing
// only a verb that named a VM.
func (server *Server) asActorHost(
	ctx context.Context, operation model.Operation, scopeKey, uuid string, run hostVerbRun,
) (model.Operation, *errorResponse) {
	recorded, failure := operation, (*errorResponse)(nil)
	err := server.reconciler.Do(ctx, scopeKey, func(ctx context.Context) error {
		var trace bytes.Buffer
		result, verbError := run(ctx, &trace)
		// Observe only a verb that named a VM, and only when it succeeded: a
		// freshly provisioned VM's state is now real and Atlas's mirror should
		// learn it, while a host-scoped verb has no VM to read. observe records
		// what the host says and never fails the verb — a verb that worked stays
		// worked even if the observation after it did not.
		if verbError == nil && uuid != "" {
			server.observe(ctx, server.newRunner(&trace), uuid, &trace)
		}
		recorded, failure = server.record(operation, &trace, result, verbError)
		return nil
	})
	if err != nil {
		return server.abandoned(operation, scopeKey, err)
	}
	return recorded, failure
}

// hostVerbError is a host verb that exited non-zero. It carries the verb's own
// exit code so the operation record mirrors the code the equivalent Task carried,
// and the last line of its trace as the one-sentence error — the verb already
// wrote the detail to its stderr, which is the operation's Output.
type hostVerbError struct {
	verb   string
	code   int
	detail string
}

func (verbError *hostVerbError) Error() string {
	if verbError.detail != "" {
		return verbError.detail
	}
	return fmt.Sprintf("host verb %s exited %d", verbError.verb, verbError.code)
}

// hostVerbVariable reads one scalar variable — the UUID a VM-scoped verb names.
// A list value has no scalar reading and returns empty, which is correct: no
// host verb names its VM through a repeatable flag.
func hostVerbVariable(variables *map[string]interface{}, key string) string {
	if variables == nil {
		return ""
	}
	value, found := (*variables)[key]
	if !found {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// hostVerbArguments renders the variables map to the verb's flag argv, the same
// UPPER_SNAKE → --kebab-case rendering _ssh/runner.py._variables_to_flags does on
// the SSH path: a scalar becomes one flag, a list becomes a repeated flag, an
// empty or absent value is dropped so the flag's own default applies. Sorted, so
// a verb's command line and trace read the same across runs.
func hostVerbArguments(variables *map[string]interface{}) []string {
	if variables == nil {
		return nil
	}
	keys := make([]string, 0, len(*variables))
	for key := range *variables {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	arguments := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		flag := "--" + strings.ReplaceAll(strings.ToLower(key), "_", "-")
		switch value := (*variables)[key].(type) {
		case string:
			if value != "" {
				arguments = append(arguments, flag, value)
			}
		case []interface{}:
			for _, item := range value {
				arguments = append(arguments, flag, fmt.Sprint(item))
			}
		case nil:
			// dropped — the flag's default applies, mirroring the SSH path
		default:
			// A JSON number or bool the variables dict would have carried as a
			// string; render it as text so the flag reads the same either way.
			arguments = append(arguments, flag, fmt.Sprint(value))
		}
	}
	return arguments
}

// parseResultLine splits a host verb's stdout into its one typed result and the
// rest. The ATLAS_RESULT= line becomes the operation's `result`; every other line
// is human trace to fold back into Output. No marker leaves the result nil —
// which is most verbs — and a caller reads that the same way it read an SSH Task
// that printed no result line.
//
// A marker whose payload will not parse is dropped rather than raised: the verbs
// emit it through json.Marshal, so a malformed line is not a case the contract
// has to answer for, and losing a result Atlas reads through parse_optional_result
// is safer than failing a verb whose host work already succeeded.
func parseResultLine(stdout string) (model.OperationResult, string) {
	var remainder strings.Builder
	var result model.OperationResult
	for _, line := range strings.Split(stdout, "\n") {
		if payload, found := strings.CutPrefix(line, resultMarker); found {
			parsed := map[string]any{}
			if err := json.Unmarshal([]byte(payload), &parsed); err == nil {
				result = parsed
			}
			continue
		}
		if line != "" {
			remainder.WriteString(line)
			remainder.WriteString("\n")
		}
	}
	return result, remainder.String()
}

// lastLine is the trace's final non-empty line — the sentence a failed verb ended
// on, which is the closest thing it has to the one-line error the lifecycle verbs
// return. The full trace is the operation's Output either way.
func lastLine(text string) string {
	trimmed := strings.TrimRight(text, "\n")
	if index := strings.LastIndex(trimmed, "\n"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}
