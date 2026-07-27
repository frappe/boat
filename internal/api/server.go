// Package api is Boat's HTTP surface: the contract generated from
// api/openapi.yaml, implemented over the operation journal and the VM manager.
//
// Everything Boat can do is an endpoint here. The daemon's two listeners and
// the operator CLI are all clients of this one implementation, so a break-glass
// command can never hold a power the API lacks — which is what makes the CLI a
// truthful tool rather than a second implementation of the host's mechanics.
//
// Two of the operations define the relationship with Atlas: PUT /vms/{uuid} is
// Atlas re-asserting intent, and GET /export is Boat re-asserting fact. Run back
// to back they resynchronize a host from any state, which is why WO-1 could
// delete a polling sweep rather than add to it.
//
// The handlers take interfaces rather than the concrete store and manager so
// they can be exercised against fakes: an HTTP test here needs neither a bbolt
// file nor a host with systemd on it.
package api

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/frappe/boat/internal/hostfacts"
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/version"
	"github.com/frappe/boat/internal/vm"
	"github.com/frappe/boat/internal/watch"
	"github.com/frappe/boat/internal/wire"
)

// OperationStore is the slice of the store the handlers need.
type OperationStore interface {
	ClaimOperation(identifier, verb, uuid string) (model.Operation, bool, error)
	CompleteOperation(operation model.Operation) error
	GetOperation(identifier string) (model.Operation, bool, error)
	PutVirtualMachine(record model.VirtualMachine) error
	GetVirtualMachine(uuid string) (model.VirtualMachine, bool, error)
	ListVirtualMachines() ([]model.VirtualMachine, error)
}

// StateStore is the slice of the store that holds the two states Boat keeps
// apart: what Atlas asked for, with the fence that permits this host to act on
// it, and what the host has been observed to be. One implementation serves both
// this and OperationStore, because a snapshot and the epoch it was taken at have
// to come out of one transaction to mean anything.
type StateStore interface {
	PutDesired(record model.DesiredVirtualMachine) error
	GetDesired(uuid string) (model.DesiredVirtualMachine, bool, error)
	SetFenceEpoch(uuid string, epoch int64) error
	FenceEpoch(uuid string) (int64, bool, error)
	ObservedEpoch() (int64, error)
	Snapshot() (model.Export, error)
}

// VirtualMachines is the slice of the VM manager the handlers need.
type VirtualMachines interface {
	Start(ctx context.Context, runner *run.Runner, uuid string) (bool, error)
	Stop(ctx context.Context, runner *run.Runner, uuid string, request vm.StopRequest) error
	Observe(ctx context.Context, runner *run.Runner, uuid string) (model.VirtualMachine, error)
	Exists(ctx context.Context, runner *run.Runner, uuid string) bool
}

// Dependencies are the collaborators a Server answers with.
//
// They are a struct rather than positional parameters because WO-1 took the
// count past what a call site can be read for: three stores-and-managers in a
// row, all interfaces, is a place where two arguments swap and nothing complains
// until a host misbehaves.
type Dependencies struct {
	Operations      OperationStore
	State           StateStore
	VirtualMachines VirtualMachines
	// Reconciler is what every verb takes its turn from, so a verb and a
	// reconcile pass can never drive one machine at once. A nil Reconciler is
	// legal and means the Server serializes against itself — see localSerializer
	// for what that costs and why the daemon never does it.
	Reconciler Reconciler
	// Watch is where observed changes are announced. A nil hub is legitimate:
	// watch carries freshness and the export carries truth, so a Server built
	// without one serves a stream that says nothing rather than dereferencing nil
	// in the middle of a verb.
	Watch     *watch.Hub
	StartedAt time.Time
}

// Server answers every documented operation.
type Server struct {
	operations      OperationStore
	state           StateStore
	virtualMachines VirtualMachines
	reconciler      Reconciler
	watch           *watch.Hub
	startedAt       time.Time
	// newRunner builds the runner a verb traces through. It is a field rather
	// than a direct call to run.NewRunner because the runner's trace writer and
	// the operation record have to be the same buffer, and because a test needs
	// to write into that buffer the way a real verb does.
	newRunner func(trace io.Writer) *run.Runner
	// hostFacts is a field for the same reason: an export has to be answerable in
	// a test that has no host under it.
	hostFacts func(ctx context.Context, runner *run.Runner) (model.HostFacts, error)
}

// The generated contract is the compile-time check that nothing here drifts
// from api/openapi.yaml.
var _ wire.StrictServerInterface = (*Server)(nil)

// NewServer builds the surface. StartedAt is the daemon's start time, which
// /host reports so a restart is visible to Atlas.
func NewServer(dependencies Dependencies) *Server {
	hub := dependencies.Watch
	if hub == nil {
		hub = watch.NewHub()
	}
	// A missing Reconciler is substituted rather than tolerated: the handlers
	// have exactly one path to the host and it goes through a turn, so there is
	// no branch anywhere below that could run a verb outside one.
	serializer := dependencies.Reconciler
	if serializer == nil {
		serializer = newLocalSerializer()
	}
	return &Server{
		operations:      dependencies.Operations,
		state:           dependencies.State,
		virtualMachines: dependencies.VirtualMachines,
		reconciler:      serializer,
		watch:           hub,
		startedAt:       dependencies.StartedAt,
		newRunner:       run.NewRunner,
		hostFacts:       hostfacts.Read,
	}
}

// GetHealth is unauthenticated by design, so a supervisor can probe a Boat that
// has not been handed a token yet. It therefore says only that Boat is up and
// which build is up — nothing about the host or its VMs.
func (server *Server) GetHealth(ctx context.Context, request wire.GetHealthRequestObject) (wire.GetHealthResponseObject, error) {
	return wire.GetHealth200JSONResponse{Status: wire.HealthStatusOk, BoatVersion: version.Version}, nil
}

// GetHost reports the observed facts of this host. boat_version is here rather
// than on a bookkeeping channel of its own so that version drift is ordinary
// observed state.
func (server *Server) GetHost(ctx context.Context, request wire.GetHostRequestObject) (wire.GetHostResponseObject, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return internalFault("This host's name could not be read.", err), nil
	}
	records, err := server.operations.ListVirtualMachines()
	if err != nil {
		return internalFault("The virtual machine records could not be read.", err), nil
	}
	return wire.GetHost200JSONResponse{
		Hostname:            hostname,
		BoatVersion:         version.Version,
		StartedAt:           server.startedAt,
		VirtualMachineCount: len(records),
	}, nil
}

// ListVirtualMachines returns what the host was last observed to hold. The
// records are written by the verbs that touched them, so this is observation
// and not a replay of what Atlas asked for.
func (server *Server) ListVirtualMachines(ctx context.Context, request wire.ListVirtualMachinesRequestObject) (wire.ListVirtualMachinesResponseObject, error) {
	records, err := server.operations.ListVirtualMachines()
	if err != nil {
		return internalFault("The virtual machine records could not be read.", err), nil
	}
	return wire.ListVirtualMachines200JSONResponse(virtualMachinesToWire(records)), nil
}

func (server *Server) GetVirtualMachine(ctx context.Context, request wire.GetVirtualMachineRequestObject) (wire.GetVirtualMachineResponseObject, error) {
	record, found, err := server.operations.GetVirtualMachine(request.Uuid)
	if err != nil {
		return internalFault("The virtual machine record could not be read.", err), nil
	}
	if !found {
		return notFound("This host has not observed a virtual machine " + request.Uuid + "."), nil
	}
	return wire.GetVirtualMachine200JSONResponse(virtualMachineToWire(record)), nil
}

// GetOperation is crash-recovery truth: it answers what the journal recorded
// under an Atlas Task name, whether or not the caller that started it survived.
func (server *Server) GetOperation(ctx context.Context, request wire.GetOperationRequestObject) (wire.GetOperationResponseObject, error) {
	operation, found, err := server.operations.GetOperation(request.OperationId)
	if err != nil {
		return internalFault("The operation journal could not be read.", err), nil
	}
	if !found {
		return notFound("This host has no operation " + request.OperationId + "."), nil
	}
	return wire.GetOperation200JSONResponse(operationToWire(operation)), nil
}
