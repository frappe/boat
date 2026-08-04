// The one test that puts a verb and the reconciler on the same store, because
// the defect it covers lives between them and neither package can see it alone.
//
// Terminate destroys a VM. The sweep drives every VM Atlas has asserted state
// for. Before the terminate retracted that assertion, the two composed into a
// loop nothing could stop: the sweep observed Stopped against desired Running,
// passed the fence — which the terminate never touched — and ran `systemctl
// start` on a VM whose root volume had just been lvremoved. Both components
// passed their own tests throughout.

package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/frappe/boat/internal/api"
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/netapply/reservedip"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/vm"
	"github.com/frappe/boat/internal/wire"
)

const (
	terminatedUuid = "1f8e0a2c-0000-4000-8000-00000000000b"
	survivingUuid  = "1f8e0a2c-0000-4000-8000-00000000000c"
)

// fakeHost is vm.Manager's whole surface without systemd, LVM or Firecracker,
// and it models the one behaviour this test is about: a terminated VM stays
// terminated. Start records who was started, so "nothing tried to boot the VM we
// destroyed" is an assertion rather than an absence nobody looked for.
type fakeHost struct {
	mutex      sync.Mutex
	status     map[string]model.VirtualMachineStatus
	started    []string
	terminated map[string]bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		status:     map[string]model.VirtualMachineStatus{},
		terminated: map[string]bool{},
	}
}

func (fake *fakeHost) Start(ctx context.Context, runner *run.Runner, uuid string) (bool, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.started = append(fake.started, uuid)
	fake.status[uuid] = model.StatusRunning
	return false, nil
}

func (fake *fakeHost) Stop(ctx context.Context, runner *run.Runner, uuid string, request vm.StopRequest) error {
	fake.set(uuid, model.StatusStopped)
	return nil
}

func (fake *fakeHost) Pause(ctx context.Context, runner *run.Runner, uuid string) error  { return nil }
func (fake *fakeHost) Resume(ctx context.Context, runner *run.Runner, uuid string) error { return nil }

func (fake *fakeHost) Sleep(
	ctx context.Context, runner *run.Runner, uuid string, request vm.SleepRequest,
) (vm.SleepResult, error) {
	fake.set(uuid, model.StatusSleeping)
	return vm.SleepResult{}, nil
}

func (fake *fakeHost) Wake(ctx context.Context, runner *run.Runner, uuid string) error {
	fake.set(uuid, model.StatusRunning)
	return nil
}

func (fake *fakeHost) Resize(
	ctx context.Context, runner *run.Runner, uuid string, request vm.ResizeRequest,
) error {
	return nil
}

func (fake *fakeHost) Rebuild(
	ctx context.Context, runner *run.Runner, uuid string, request vm.RebuildRequest,
) error {
	return nil
}

func (fake *fakeHost) ReservedIP(
	ctx context.Context, runner *run.Runner, uuid string, request vm.ReservedIPRequest,
) (reservedip.Delivery, error) {
	return reservedip.Delivery{}, nil
}

func (fake *fakeHost) InjectIdentity(
	ctx context.Context, runner *run.Runner, device string, uuid string, identity vm.Identity,
) error {
	return nil
}

// Terminate is the real thing's shape: the unit and the volumes are gone
// afterwards, so the VM reads Stopped forever and a start would have nothing to
// boot.
func (fake *fakeHost) Terminate(ctx context.Context, runner *run.Runner, uuid string) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.terminated[uuid] = true
	fake.status[uuid] = model.StatusStopped
	return nil
}

func (fake *fakeHost) FirecrackerUID(ctx context.Context, runner *run.Runner, uuid string) (int, error) {
	return 200001, nil
}

func (fake *fakeHost) Observe(
	ctx context.Context, runner *run.Runner, uuid string,
) (model.VirtualMachine, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	status, found := fake.status[uuid]
	if !found {
		status = model.StatusStopped
	}
	return model.VirtualMachine{UUID: uuid, ObservedStatus: status, ObservedAt: time.Now().UTC()}, nil
}

// Exists is what the API asks before every verb. A terminated VM's directory is
// gone, so this answers the way a real host does afterwards.
func (fake *fakeHost) Exists(ctx context.Context, runner *run.Runner, uuid string) bool {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return !fake.terminated[uuid]
}

func (fake *fakeHost) set(uuid string, status model.VirtualMachineStatus) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.status[uuid] = status
}

func (fake *fakeHost) startsOf(uuid string) int {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	count := 0
	for _, started := range fake.started {
		if started == uuid {
			count++
		}
	}
	return count
}

// A terminate leaves nothing for the sweep to start, and the VM beside it proves
// the sweep ran at all — without that control, a reconciler wired to nothing
// would pass this test.
func TestASweepStartsNothingForAVirtualMachineTerminateRetracted(t *testing.T) {
	host := newFakeHost()
	parts := newTestParts(t, host)
	dependencies := parts.dependencies()
	dependencies.VirtualMachines = host
	server := api.NewServer(dependencies)
	desire(t, parts, terminatedUuid)
	desire(t, parts, survivingUuid)

	response, err := server.TerminateVirtualMachine(context.Background(),
		wire.TerminateVirtualMachineRequestObject{
			Uuid: terminatedUuid,
			Body: &wire.OperationRequest{OperationId: "Task-terminate-sweep"},
		})
	if err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if _, accepted := response.(wire.TerminateVirtualMachine200JSONResponse); !accepted {
		t.Fatalf("terminate answered %T, want the operation record", response)
	}

	lifetime, stop := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		parts.reconciler.Run(lifetime)
	}()
	// The sweep is the first thing Run does, so waiting for the surviving VM to
	// come up is waiting for a sweep that has seen both records.
	waitUntil(t, "the sweep to start the virtual machine Atlas still wants", func() bool {
		return host.startsOf(survivingUuid) > 0
	})
	stop()
	<-finished

	if starts := host.startsOf(terminatedUuid); starts != 0 {
		t.Errorf("the sweep started a terminated virtual machine %d times", starts)
	}
	if _, found, err := parts.store.GetDesired(terminatedUuid); found || err != nil {
		t.Errorf("the terminated VM is still desired: found=%v err=%v", found, err)
	}
	// The epoch is kept: retraction ends this host's authority to act on the VM,
	// and must not also hand back permission to boot it under an older claim.
	if epoch, held, err := parts.store.FenceEpoch(terminatedUuid); !held || epoch != 1 || err != nil {
		t.Errorf("fence epoch = %d held=%v err=%v, want the 1 it already held", epoch, held, err)
	}
}

// desire is Atlas asserting Running with the epoch that permits this host to
// boot it — the state a terminate has to take back.
func desire(t *testing.T, parts *daemonParts, uuid string) {
	t.Helper()
	if err := parts.store.SetFenceEpoch(uuid, 1); err != nil {
		t.Fatalf("could not assert the fence epoch: %v", err)
	}
	record := model.DesiredVirtualMachine{UUID: uuid, BootEpoch: 1, DesiredPower: model.PowerRunning}
	if err := parts.store.PutDesired(record); err != nil {
		t.Fatalf("could not assert the desired state: %v", err)
	}
}

func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
