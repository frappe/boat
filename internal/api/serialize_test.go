package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/wire"
)

// recordingReconciler is the serialization contract in miniature — one turn per
// UUID, exactly as reconcile.Do gives — plus a record of what it was asked to
// serialize and, when refuse is set, a turn nobody gets.
type recordingReconciler struct {
	mutex  sync.Mutex
	turns  map[string]*sync.Mutex
	asked  []string
	refuse error
}

func newRecordingReconciler() *recordingReconciler {
	return &recordingReconciler{turns: map[string]*sync.Mutex{}}
}

func (fake *recordingReconciler) Do(ctx context.Context, uuid string, fn func(context.Context) error) error {
	turn := fake.turnFor(uuid)
	if fake.refuse != nil {
		return fake.refuse
	}
	turn.Lock()
	defer turn.Unlock()
	return fn(ctx)
}

func (fake *recordingReconciler) turnFor(uuid string) *sync.Mutex {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.asked = append(fake.asked, uuid)
	turn, found := fake.turns[uuid]
	if !found {
		turn = &sync.Mutex{}
		fake.turns[uuid] = turn
	}
	return turn
}

func (fake *recordingReconciler) served() []string {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return append([]string(nil), fake.asked...)
}

// A verb's work is INSIDE the turn, not beside it. A reconciler that never hands
// the turn over is how that is proved: if the verb ran anyway, the work was
// never inside.
func TestAVerbThatNeverGetsItsTurnTouchesNothing(t *testing.T) {
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	machines := &fakeVirtualMachines{}
	server := newTestServer(operations, machines)
	server.reconciler = &recordingReconciler{turns: map[string]*sync.Mutex{}, refuse: context.Canceled}

	recorder := postJSON(t, server.SocketHandler(), "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-turn"})

	if machines.starts != 0 {
		t.Errorf("the verb ran %d times outside its turn, want 0", machines.starts)
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("got %d, want 500: %s", recorder.Code, recorder.Body)
	}
}

// The claim is written before the turn is taken, so a verb that never got its
// turn still owes a terminal record: an operation left Running is one the Atlas
// Task behind it waits on forever.
func TestAVerbThatNeverGetsItsTurnIsStillJournalledTerminal(t *testing.T) {
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	server := newTestServer(operations, &fakeVirtualMachines{})
	server.reconciler = &recordingReconciler{turns: map[string]*sync.Mutex{}, refuse: context.Canceled}

	postJSON(t, server.SocketHandler(), "/vms/"+testUuid+"/start", wire.StartRequest{OperationId: "Task-abandoned"})

	recorded := operations.operations["Task-abandoned"]
	if !recorded.Finished() {
		t.Fatalf("the abandoned operation was left non-terminal: %+v", recorded)
	}
	if recorded.Status != model.OperationFailure || recorded.Error == "" {
		t.Errorf("the abandoned operation carried no reason: %+v", recorded)
	}
}

// Two ordinary requests — a stop and a start for one UUID — arrive on two
// goroutines. Unserialized they interleave vm-network-down with vm-network-up,
// and both still report success, which is why the fake watches for the overlap
// rather than for a count.
func TestAStopAndAStartForOneVirtualMachineNeverOverlap(t *testing.T) {
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	machines := &fakeVirtualMachines{hold: 20 * time.Millisecond}
	server := newTestServer(operations, machines)
	reconciler := newRecordingReconciler()
	server.reconciler = reconciler

	startAndStopTogether(t, server.SocketHandler())

	if machines.everOverlapped() {
		t.Error("a start and a stop drove one virtual machine at the same time")
	}
	if machines.starts != 1 || machines.stops != 1 {
		t.Errorf("got %d starts and %d stops, want one of each", machines.starts, machines.stops)
	}
	if served := reconciler.served(); len(served) != 2 || served[0] != testUuid || served[1] != testUuid {
		t.Errorf("the verbs did not both go through the reconciler: %v", served)
	}
}

// A nil Reconciler is legal and never means "run unserialized": the Server
// serializes against itself instead. What it cannot do without a reconciler is
// exclude a reconcile pass, which is why the daemon always passes one.
func TestANilReconcilerStillSerializesTheVerbs(t *testing.T) {
	operations := newFakeStore()
	operations.fence(testUuid, 1)
	machines := &fakeVirtualMachines{hold: 20 * time.Millisecond}
	server := newTestServer(operations, machines)

	if _, local := server.reconciler.(*localSerializer); !local {
		t.Fatalf("a Server built without a Reconciler got %T, want a localSerializer", server.reconciler)
	}
	startAndStopTogether(t, server.SocketHandler())

	if machines.everOverlapped() {
		t.Error("a start and a stop drove one virtual machine at the same time")
	}
}

// Turns are per VM: one guest's boot must never be another guest's queue.
func TestOneVirtualMachinesTurnDoesNotHoldAnother(t *testing.T) {
	serializer := newLocalSerializer()
	held := make(chan struct{})
	free := make(chan struct{})
	go func() {
		_ = serializer.Do(context.Background(), "first", func(context.Context) error {
			close(held)
			<-free
			return nil
		})
	}()
	<-held
	defer close(free)

	done := make(chan error, 1)
	go func() {
		done <- serializer.Do(context.Background(), "second", func(context.Context) error { return nil })
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the second virtual machine's turn failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("one virtual machine's turn held another's")
	}
}

// A caller that queued behind a boot and was cancelled meanwhile must not then
// drive the host.
func TestATurnGivenToACancelledCallerRunsNothing(t *testing.T) {
	serializer := newLocalSerializer()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false

	err := serializer.Do(ctx, testUuid, func(context.Context) error {
		ran = true
		return nil
	})

	if ran {
		t.Error("a cancelled caller was allowed to drive the host")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want the cancellation", err)
	}
}

// startAndStopTogether posts both verbs at once. The bodies are encoded on this
// goroutine because the request helper reports its failures through t, which
// only the test's own goroutine may do.
func startAndStopTogether(t *testing.T, handler http.Handler) {
	t.Helper()
	start := encode(t, wire.StartRequest{OperationId: "Task-start"})
	stop := encode(t, wire.StopRequest{OperationId: "Task-stop"})
	var together sync.WaitGroup
	for path, body := range map[string]string{"/start": start, "/stop": stop} {
		together.Add(1)
		go func() {
			defer together.Done()
			postBody(handler, "/vms/"+testUuid+path, body)
		}()
	}
	together.Wait()
}

func encode(t *testing.T, body any) string {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("could not encode the request body: %v", err)
	}
	return string(encoded)
}
