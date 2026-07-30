package units

import (
	"context"
	"fmt"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
)

// commands is everything this package does to the host, and the only seam it
// has. Outside tests there is one implementation, *run.Runner, and there is
// never a second.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
}

var _ commands = (*run.Runner)(nil)

// Supervisor reports on and acts on this host's sibling units.
//
// It holds no state between calls. systemd is the state, and asking it is the
// whole job — a cached ActiveState is a claim about a unit that may have died
// since, which is the class of defect Boat exists to remove from Atlas.
type Supervisor struct {
	commandsFor func(runner *run.Runner) commands
}

// NewSupervisor returns a Supervisor wired to the real host.
func NewSupervisor() *Supervisor {
	return &Supervisor{commandsFor: func(runner *run.Runner) commands { return runner }}
}

// Liveness reports every supervised unit this host actually has.
//
// The slice is empty rather than nil when the host has none of them, and the
// difference is load-bearing all the way out to the export: an empty array says
// "asked, and this host runs none", while an absent one would say "not looked
// at" (§2.5). A bare machine that has never been bootstrapped is the first case,
// and it is a fact worth reporting.
func (supervisor *Supervisor) Liveness(ctx context.Context, runner *run.Runner) ([]model.UnitLiveness, error) {
	return supervisor.read(ctx, supervisor.commandsFor(runner), supervised)
}

// LivenessOf reports one supervised unit, and found=false when systemd says
// this host does not have it. Absence is an answer, not a failure.
//
// A name outside the supervised set is an error rather than an empty answer.
// The caller has asked about something this host will not discuss, and saying
// "not found" would blur that into "installed nowhere" — two different facts,
// and only one of them is about the unit.
func (supervisor *Supervisor) LivenessOf(
	ctx context.Context, runner *run.Runner, name string,
) (model.UnitLiveness, bool, error) {
	if !IsSupervised(name) {
		return model.UnitLiveness{}, false, unsupervised(name)
	}
	found, err := supervisor.read(ctx, supervisor.commandsFor(runner), []string{name})
	if err != nil || len(found) == 0 {
		return model.UnitLiveness{}, false, err
	}
	return found[0], true, nil
}

// Act starts or restarts one supervised unit.
//
// Both gates are re-checked here even though internal/api checks them first,
// and that is not belt-and-braces for its own sake: this is the last point
// before a unit name becomes a sudo argument, and the next caller of this
// package will be inside the daemon rather than behind the API — the update
// sequence of §5, which restarts the sibling units after a binary swap.
func (supervisor *Supervisor) Act(
	ctx context.Context, runner *run.Runner, name string, action Action,
) error {
	if !IsSupervised(name) {
		return unsupervised(name)
	}
	template, known := commandFor(action)
	if !known {
		return fmt.Errorf("this host performs no %q on a unit", action)
	}
	_, err := supervisor.commandsFor(runner).Run(ctx, template, name)
	return err
}

func unsupervised(name string) error {
	return fmt.Errorf("this host supervises no unit named %q", name)
}
