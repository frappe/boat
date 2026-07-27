package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/store"
	"github.com/frappe/boat/internal/vm"
	"github.com/frappe/boat/internal/wire"
)

// fakeOperationStore is the journal without bbolt. ClaimOperation keeps the
// real rule — a second claim of one identifier is a replay, and a claim against
// different work is a conflict — because the handlers' idempotency is exactly
// what these tests are about.
type fakeOperationStore struct {
	operations      map[string]model.Operation
	virtualMachines map[string]model.VirtualMachine
	claimError      error
	completeError   error
	readError       error
}

func newFakeOperationStore() *fakeOperationStore {
	return &fakeOperationStore{
		operations:      map[string]model.Operation{},
		virtualMachines: map[string]model.VirtualMachine{},
	}
}

func (fake *fakeOperationStore) ClaimOperation(identifier, verb, uuid string) (model.Operation, bool, error) {
	if fake.claimError != nil {
		return model.Operation{}, false, fake.claimError
	}
	if existing, found := fake.operations[identifier]; found {
		if !existing.Matches(verb, uuid) {
			return model.Operation{}, false, store.ErrOperationConflict
		}
		return existing, false, nil
	}
	claimed := model.Operation{
		Identifier:         identifier,
		Verb:               verb,
		VirtualMachineUUID: uuid,
		Status:             model.OperationRunning,
		StartedAt:          time.Now().UTC(),
	}
	fake.operations[identifier] = claimed
	return claimed, true, nil
}

func (fake *fakeOperationStore) CompleteOperation(operation model.Operation) error {
	if fake.completeError != nil {
		return fake.completeError
	}
	fake.operations[operation.Identifier] = operation
	return nil
}

func (fake *fakeOperationStore) GetOperation(identifier string) (model.Operation, bool, error) {
	if fake.readError != nil {
		return model.Operation{}, false, fake.readError
	}
	operation, found := fake.operations[identifier]
	return operation, found, nil
}

func (fake *fakeOperationStore) PutVirtualMachine(record model.VirtualMachine) error {
	fake.virtualMachines[record.UUID] = record
	return nil
}

func (fake *fakeOperationStore) GetVirtualMachine(uuid string) (model.VirtualMachine, bool, error) {
	if fake.readError != nil {
		return model.VirtualMachine{}, false, fake.readError
	}
	record, found := fake.virtualMachines[uuid]
	return record, found, nil
}

func (fake *fakeOperationStore) ListVirtualMachines() ([]model.VirtualMachine, error) {
	if fake.readError != nil {
		return nil, fake.readError
	}
	records := make([]model.VirtualMachine, 0, len(fake.virtualMachines))
	for _, record := range fake.virtualMachines {
		records = append(records, record)
	}
	return records, nil
}

// fakeVirtualMachines counts what it was asked to do, so a test can prove a
// replay ran nothing at all.
type fakeVirtualMachines struct {
	trace        io.Writer
	traceText    string
	starts       int
	stops        int
	stopRequests []vm.StopRequest
	startError   error
	stopError    error
	missing      bool
	observed     model.VirtualMachine
	observeError error
}

func (fake *fakeVirtualMachines) Start(ctx context.Context, runner *run.Runner, uuid string) (bool, error) {
	fake.starts++
	fake.writeTrace()
	return false, fake.startError
}

func (fake *fakeVirtualMachines) Stop(ctx context.Context, runner *run.Runner, uuid string, request vm.StopRequest) error {
	fake.stops++
	fake.stopRequests = append(fake.stopRequests, request)
	fake.writeTrace()
	return fake.stopError
}

func (fake *fakeVirtualMachines) Observe(ctx context.Context, runner *run.Runner, uuid string) (model.VirtualMachine, error) {
	if fake.observeError != nil {
		return model.VirtualMachine{}, fake.observeError
	}
	observed := fake.observed
	observed.UUID = uuid
	return observed, nil
}

func (fake *fakeVirtualMachines) Exists(ctx context.Context, runner *run.Runner, uuid string) bool {
	return !fake.missing
}

// writeTrace stands in for the `+ command` lines a real verb's runner emits.
// run.Runner keeps its writer to itself, so the fake is handed the same writer
// the server gave the runner — see newTestServer.
func (fake *fakeVirtualMachines) writeTrace() {
	if fake.trace != nil && fake.traceText != "" {
		fmt.Fprint(fake.trace, fake.traceText)
	}
}

// newTestServer wires the fakes together and lets the fake verb write the
// operation's trace, which is what proves the trace reaches Operation.Output.
func newTestServer(operations *fakeOperationStore, machines *fakeVirtualMachines) *Server {
	server := NewServer(operations, machines, time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC))
	server.newRunner = func(trace io.Writer) *run.Runner {
		machines.trace = trace
		return run.NewRunner(trace)
	}
	return server
}

func postJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("could not encode the request body: %v", err)
	}
	return postBody(handler, path, string(encoded))
}

func postBody(handler http.Handler, path string, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func get(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder
}

func decodeOperation(t *testing.T, recorder *httptest.ResponseRecorder) wire.Operation {
	t.Helper()
	var operation wire.Operation
	decode(t, recorder, &operation)
	return operation
}

func decodeError(t *testing.T, recorder *httptest.ResponseRecorder) wire.Error {
	t.Helper()
	var failure wire.Error
	decode(t, recorder, &failure)
	return failure
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), into); err != nil {
		t.Fatalf("could not decode %q: %v", recorder.Body.String(), err)
	}
}
