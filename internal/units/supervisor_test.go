package units

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
)

// meoOutput is what a real host answered, captured from
// `systemctl show --property=Id --property=LoadState --property=ActiveState
// --property=SubState` over the four supervised names on a live dev host that
// runs two of them. It is the fixture rather than something invented, because
// the two behaviours that matter — not-found being dropped, and a oneshot
// reading active/exited — are only obvious from real output.
const meoOutput = `Id=atlas-pool.service
LoadState=loaded
ActiveState=active
SubState=exited

Id=atlas-networkd.service
LoadState=not-found
ActiveState=inactive
SubState=dead

Id=atlas-wake-trap.service
LoadState=loaded
ActiveState=active
SubState=running

Id=atlas-mgmt-firewall.service
LoadState=not-found
ActiveState=inactive
SubState=dead
`

// fakeCommands records what was rendered and answers with what a host would.
type fakeCommands struct {
	issued  []string
	answer  string
	failure error
}

func (fake *fakeCommands) Run(ctx context.Context, template string, parameters ...any) (string, error) {
	rendered, err := run.Substitute(template, parameters...)
	if err != nil {
		return "", err
	}
	fake.issued = append(fake.issued, rendered)
	return fake.answer, fake.failure
}

func newSupervisor(fake *fakeCommands) *Supervisor {
	return &Supervisor{commandsFor: func(runner *run.Runner) commands { return fake }}
}

// A unit systemd calls not-found is not a unit that is down; it is a service
// this host does not run. Atlas reads any unit whose active_state is not active
// as down, so reporting the absent ones would flag every host in the fleet as
// permanently degraded — and a health field that is always red is one nobody
// reads.
func TestLivenessDropsTheUnitsThisHostDoesNotHave(t *testing.T) {
	commands := &fakeCommands{answer: meoOutput}

	liveness, err := newSupervisor(commands).Liveness(context.Background(), nil)

	if err != nil {
		t.Fatalf("liveness: %v", err)
	}
	want := []model.UnitLiveness{
		{Name: "atlas-pool.service", ActiveState: "active", SubState: "exited"},
		{Name: "atlas-wake-trap.service", ActiveState: "active", SubState: "running"},
	}
	if len(liveness) != len(want) {
		t.Fatalf("got %+v, want the two units this host has", liveness)
	}
	for index, unit := range want {
		if liveness[index] != unit {
			t.Errorf("got %+v, want %+v", liveness[index], unit)
		}
	}
}

// One subprocess for the whole set. This runs on every GET /host and inside
// every export, and the export is polled per host across the fleet.
func TestLivenessAsksForEverySupervisedUnitInOneCall(t *testing.T) {
	commands := &fakeCommands{answer: meoOutput}

	if _, err := newSupervisor(commands).Liveness(context.Background(), nil); err != nil {
		t.Fatalf("liveness: %v", err)
	}

	if len(commands.issued) != 1 {
		t.Fatalf("got %d commands, want one: %v", len(commands.issued), commands.issued)
	}
	issued := commands.issued[0]
	if strings.HasPrefix(issued, "sudo ") {
		t.Error("the liveness read used sudo, and a property read over the system bus needs none")
	}
	for _, name := range supervised {
		if !strings.Contains(issued, name) {
			t.Errorf("%q did not ask about %s", issued, name)
		}
	}
}

// An empty answer is an answer: this host was asked about every supervised unit
// and runs none of them, which is true of a machine that was never bootstrapped.
// Nil would mean "not looked at" once it reaches the export.
func TestLivenessIsEmptyRatherThanNilWhenTheHostRunsNoneOfThem(t *testing.T) {
	commands := &fakeCommands{answer: ""}

	liveness, err := newSupervisor(commands).Liveness(context.Background(), nil)

	if err != nil {
		t.Fatalf("liveness: %v", err)
	}
	if liveness == nil {
		t.Fatal("got nil, which reads as unlooked-at rather than empty")
	}
	if len(liveness) != 0 {
		t.Errorf("got %+v, want none", liveness)
	}
}

func TestLivenessOfReportsOneUnitAndItsAbsence(t *testing.T) {
	commands := &fakeCommands{answer: meoOutput}
	supervisor := newSupervisor(commands)

	unit, found, err := supervisor.LivenessOf(context.Background(), nil, "atlas-pool.service")
	if err != nil || !found {
		t.Fatalf("got found=%v err=%v, want the pool", found, err)
	}
	if unit.SubState != "exited" {
		t.Errorf("got sub state %q, want a oneshot's exited", unit.SubState)
	}

	commands.answer = "Id=atlas-networkd.service\nLoadState=not-found\nActiveState=inactive\nSubState=dead\n"
	if _, found, err = supervisor.LivenessOf(context.Background(), nil, "atlas-networkd.service"); err != nil {
		t.Fatalf("reading an absent unit failed: %v", err)
	}
	if found {
		t.Error("a unit systemd calls not-found was reported as present")
	}
}

// The last gate before a unit name becomes a sudo argument. internal/api checks
// first; this is what holds for the next caller inside the daemon, which is the
// update sequence of §5.
func TestAnUnsupervisedNameNeverReachesTheHost(t *testing.T) {
	commands := &fakeCommands{answer: meoOutput}
	supervisor := newSupervisor(commands)

	actErr := supervisor.Act(context.Background(), nil, "sshd.service", Restart)
	_, _, readErr := supervisor.LivenessOf(context.Background(), nil, "sshd.service")

	if actErr == nil || readErr == nil {
		t.Fatalf("sshd was accepted: act=%v read=%v", actErr, readErr)
	}
	if len(commands.issued) != 0 {
		t.Errorf("commands reached the host anyway: %v", commands.issued)
	}
}

func TestActRendersTheActionAndTheUnitAndNothingElse(t *testing.T) {
	commands := &fakeCommands{}

	if err := newSupervisor(commands).Act(context.Background(), nil, "atlas-pool.service", Restart); err != nil {
		t.Fatalf("act: %v", err)
	}

	if len(commands.issued) != 1 || commands.issued[0] != "sudo systemctl restart atlas-pool.service" {
		t.Fatalf("got %v, want one pinned restart", commands.issued)
	}
}

func TestActRefusesAnActionThisHostDoesNotPerform(t *testing.T) {
	commands := &fakeCommands{}

	err := newSupervisor(commands).Act(context.Background(), nil, "atlas-pool.service", Action("stop"))

	if err == nil {
		t.Fatal("stop was performed")
	}
	if len(commands.issued) != 0 {
		t.Errorf("commands reached the host anyway: %v", commands.issued)
	}
}

// A failed read is returned, not swallowed into an empty set. An empty set means
// "this host runs none of them", and a host whose systemd is not answering has
// made no such claim.
func TestALivenessReadThatFailsIsNotAnEmptySet(t *testing.T) {
	commands := &fakeCommands{failure: errors.New("systemd is not answering")}

	liveness, err := newSupervisor(commands).Liveness(context.Background(), nil)

	if err == nil {
		t.Fatalf("got %+v and no error, want the failure", liveness)
	}
	if liveness != nil {
		t.Errorf("got %+v beside the error, want nothing", liveness)
	}
}
