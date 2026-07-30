package api

import (
	"context"
	"io"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/units"
	"github.com/frappe/boat/internal/wire"
)

// GetUnit reports what systemd says about one of this host's own services.
//
// The name is checked against the supervised set before anything is run. That
// check is what keeps the per-VM `firecracker-vm@` instances out of this
// endpoint: they are driven by the lifecycle verbs, behind the fence and
// through the journal, and a second door onto them here would be a door around
// all three. sshd and boat.service are excluded by the same list, and by being
// a list of four literal names it excludes them without anyone having written a
// rule about them.
func (server *Server) GetUnit(ctx context.Context, request wire.GetUnitRequestObject) (wire.GetUnitResponseObject, error) {
	liveness, failure := server.unitLiveness(ctx, request.Name)
	if failure != nil {
		return failure, nil
	}
	return wire.GetUnit200JSONResponse(unitToWire(liveness)), nil
}

// ActOnUnit starts or restarts one of this host's own services.
//
// There is no stop, and the absence is the design rather than a first cut — see
// internal/units. What arrives here is therefore always convergent: both actions
// leave the unit running, or fail saying why.
//
// The liveness is read back afterwards and returned, so the caller learns what
// the unit became rather than that a command was accepted. `systemctl start`
// and `systemctl restart` return once the job has finished, so the read is not
// a race with it.
func (server *Server) ActOnUnit(ctx context.Context, request wire.ActOnUnitRequestObject) (wire.ActOnUnitResponseObject, error) {
	if request.Body == nil {
		return badRequest("This request needs an action."), nil
	}
	action, known := units.ParseAction(string(request.Body.Action))
	if !known {
		return badRequest("This host performs no " + string(request.Body.Action) + " on a unit."), nil
	}
	// The name is validated by the same read that answers a GET, so an
	// unsupervised or uninstalled unit is refused before anything is asked of
	// systemd — acting on a unit this host does not have would fail anyway, but it
	// would fail as a command error rather than as the plain fact that it is not
	// here.
	if _, failure := server.unitLiveness(ctx, request.Name); failure != nil {
		return failure, nil
	}
	if err := server.units.Act(ctx, server.newRunner(io.Discard), request.Name, action); err != nil {
		return internalFault("This host could not "+string(action)+" "+request.Name+".", err), nil
	}
	liveness, failure := server.unitLiveness(ctx, request.Name)
	if failure != nil {
		return failure, nil
	}
	return wire.ActOnUnit200JSONResponse(unitToWire(liveness)), nil
}

// unitLiveness reads one supervised unit, refusing the two ways a name can fail
// to name one.
//
// They are different facts and the sentences say so. "This host supervises no
// unit named X" is about the closed list Boat was built with; "this host does
// not have X installed" is about the machine. A caller that conflated them would
// read a host missing its network daemon as a Boat that had never heard of one.
func (server *Server) unitLiveness(ctx context.Context, name string) (model.UnitLiveness, *errorResponse) {
	if !units.IsSupervised(name) {
		return model.UnitLiveness{}, notFound("This host supervises no unit named " + name + ".")
	}
	liveness, found, err := server.units.LivenessOf(ctx, server.newRunner(io.Discard), name)
	if err != nil {
		return model.UnitLiveness{}, internalFault("The unit "+name+" could not be read.", err)
	}
	if !found {
		return model.UnitLiveness{}, notFound("This host does not have " + name + " installed.")
	}
	return liveness, nil
}
