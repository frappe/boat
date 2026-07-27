package reconcile

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/frappe/boat/internal/model"
)

// The invariant the package exists for, asserted rather than assumed: many
// goroutines drive one VM through both doors at once — verbs through Do and
// passes through Wake — and the fake host counts any moment two of them are
// inside it. Run under -race, this is what proves the actor is one.
//
// Both resting states are here because they take different steps. The sleeping
// row is the wake path, and it is the one that has to be nailed down: the trap
// can knock on it many times a second from an unauthenticated packet, so a wake
// that reached the host outside the actor would be the cheapest way in the whole
// system to get two drivers onto one VM.
func TestOneActorPerVirtualMachineUnderConcurrentVerbsAndPasses(t *testing.T) {
	for name, resting := range map[string]struct {
		status model.VirtualMachineStatus
		verb   string
	}{
		"a stopped virtual machine":  {model.StatusStopped, "start"},
		"a sleeping virtual machine": {model.StatusSleeping, "wake"},
	} {
		t.Run(name, func(t *testing.T) {
			harness := newReconciler(t)
			harness.desire(t, firstVirtualMachine, model.PowerRunning)
			harness.machines.setStatus(firstVirtualMachine, resting.status)

			const goroutines = 32
			var workers sync.WaitGroup
			for worker := 0; worker < goroutines; worker++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					// A verb, occupying the VM exactly as one that touched the host would.
					err := harness.reconciler.Do(context.Background(), firstVirtualMachine, func(ctx context.Context) error {
						harness.occupancy.enter(firstVirtualMachine)
						defer harness.occupancy.exit(firstVirtualMachine)
						return nil
					})
					if err != nil {
						t.Errorf("verb: %v", err)
					}
					harness.reconciler.Wake(firstVirtualMachine)
				}()
			}
			workers.Wait()
			harness.settled(t)

			if overlaps := harness.occupancy.overlaps(); overlaps != 0 {
				t.Fatalf("%d overlaps: a verb and a pass drove one virtual machine at the same time", overlaps)
			}
			if harness.machines.counted(resting.verb) == 0 {
				t.Fatalf("no pass reached the host with a %s, so the exclusion above proved nothing", resting.verb)
			}
		})
	}
}

// One actor per VM, not one per host. Eight VMs each hold their actor at once;
// a reconciler with a single lock would let one in and then wait for a release
// that never comes, and the deadline says which it was.
func TestDifferentVirtualMachinesProceedConcurrently(t *testing.T) {
	harness := newReconciler(t)
	const count = 8
	entered := make(chan struct{}, count)
	release := make(chan struct{})
	var workers sync.WaitGroup
	for index := 0; index < count; index++ {
		uuid := fmt.Sprintf("virtual-machine-%02d", index)
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := harness.reconciler.Do(context.Background(), uuid, func(ctx context.Context) error {
				entered <- struct{}{}
				<-release
				return nil
			})
			if err != nil {
				t.Errorf("verb on %s: %v", uuid, err)
			}
		}()
	}
	for admitted := 0; admitted < count; admitted++ {
		select {
		case <-entered:
		case <-time.After(testDeadline):
			t.Fatalf("only %d of %d virtual machines ran at once: the actor has become a host-wide lock", admitted, count)
		}
	}
	close(release)
	workers.Wait()
}

// Wake is called from an HTTP handler and from the wake trap's poll loop.
// Neither may wait for a guest to boot, and neither may run the pass itself —
// running it on the caller's goroutine would also put it outside the actor.
func TestWakeNeitherBlocksNorRunsThePassItself(t *testing.T) {
	harness := newReconciler(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)

	holding, release := make(chan struct{}), make(chan struct{})
	go harness.reconciler.Do(context.Background(), firstVirtualMachine, func(ctx context.Context) error {
		close(holding)
		<-release
		return nil
	})
	<-holding

	returned := make(chan struct{})
	go func() {
		harness.reconciler.Wake(firstVirtualMachine)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(testDeadline):
		t.Fatal("Wake blocked while the virtual machine's actor was busy")
	}
	if count := harness.machines.counted("observe"); count != 0 {
		t.Fatalf("the pass touched the host %d times while a verb held the actor", count)
	}

	close(release)
	waitFor(t, "the queued pass to run once the verb let go", func() bool {
		return harness.machines.counted("observe") > 0
	})
}

// A pass that fails must not spin. Each failure costs the VM a longer wait, and
// the wait is asserted from the delays the reconciler asked for rather than by
// living through them.
func TestAFailingPassBacksOffInsteadOfSpinning(t *testing.T) {
	harness := newReconciler(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	harness.machines.refuse("observe", errHostRefused)

	expected := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond}
	for attempt, want := range expected {
		if got := harness.wakeAndSettle(t); got != want {
			t.Fatalf("delay after failure %d = %v, want %v", attempt+1, got, want)
		}
	}
}

// A VM that recovers is reconciled at full speed again, rather than serving out
// the backoff its last failure earned.
func TestBackoffClearsAfterAPassSucceeds(t *testing.T) {
	harness := newReconciler(t)
	harness.desire(t, firstVirtualMachine, model.PowerRunning)
	harness.machines.refuse("observe", errHostRefused)
	harness.wakeAndSettle(t)
	if got := harness.wakeAndSettle(t); got != 20*time.Millisecond {
		t.Fatalf("second delay = %v, want the backoff to have doubled", got)
	}

	harness.machines.refuse("observe", nil)
	harness.reconciler.Wake(firstVirtualMachine)
	waitFor(t, "the virtual machine to start once the host answers", func() bool {
		return harness.machines.counted("start") > 0
	})
	harness.settled(t)

	harness.machines.refuse("observe", errHostRefused)
	if got := harness.wakeAndSettle(t); got != 10*time.Millisecond {
		t.Fatalf("delay after a recovery = %v, want the backoff to have started over", got)
	}
}

// wakeAndSettle asks for one pass and returns the delay the reconciler wanted
// after it. It waits for the pass to reach the host first, so each call is
// exactly one attempt rather than a coalesced pile of them.
func (harness *harness) wakeAndSettle(t *testing.T) time.Duration {
	t.Helper()
	before := harness.machines.counted("observe")
	harness.reconciler.Wake(firstVirtualMachine)
	waitFor(t, "the pass to reach the host", func() bool {
		return harness.machines.counted("observe") > before
	})
	select {
	case delay := <-harness.delays:
		return delay
	case <-time.After(testDeadline):
		t.Fatal("the reconciler never backed off after a failed pass")
		return 0
	}
}

func TestBackoffDelay(t *testing.T) {
	policy := backoff{base: time.Second, max: 5 * time.Minute}
	cases := map[int]time.Duration{
		0:    0,
		1:    time.Second,
		2:    2 * time.Second,
		3:    4 * time.Second,
		9:    256 * time.Second,
		10:   5 * time.Minute,
		1000: 5 * time.Minute,
	}
	for failures, want := range cases {
		if got := policy.delay(failures); got != want {
			t.Errorf("delay after %d failures = %v, want %v", failures, got, want)
		}
	}
}
