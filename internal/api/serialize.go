package api

import (
	"context"
	"sync"
)

// Reconciler is the slice of the reconciler the handlers need: the one point a
// VM's work is serialized at.
//
// Every verb runs its work inside Do, so a verb and a reconcile pass can never
// drive one machine at once. Without it two ordinary requests — a stop and a
// start for one UUID — arrive on two goroutines and interleave
// `vm-network-down` with `vm-network-up`, and the state that leaves behind is
// not one either of them intended.
//
// It is one method because that is all a handler needs. The reconciler's other
// powers — requesting a pass, running the sweep — belong to the daemon that
// built it, not to the boundary that answers requests.
type Reconciler interface {
	Do(ctx context.Context, uuid string, fn func(context.Context) error) error
}

// localSerializer is what a Server built without a Reconciler serializes with.
//
// A nil Dependencies.Reconciler is legal, and it does NOT mean "run
// unserialized". It means there is no reconciler in this process — a handler
// test, a Server built by a tool that drives nothing else — so this Server is
// the only thing touching the host and it serializes against itself. The one
// guarantee it cannot give is the one that needs a reconciler: that a verb
// excludes a reconcile pass. `boat daemon` therefore always passes one, and
// cmd/boat has a test that says so, because the difference between a Server
// with a reconciler and a Server without one is invisible until a host
// misbehaves.
//
// It mirrors reconcile.Do, including the context check after the wait rather
// than before it: a caller that queued behind a two-minute boot and was
// cancelled meanwhile must not then drive the host.
type localSerializer struct {
	// mutex guards turns and nothing else. It is never held while fn runs, or
	// one VM's boot would be every other VM's queue.
	mutex sync.Mutex
	turns map[string]*sync.Mutex
}

func newLocalSerializer() *localSerializer {
	return &localSerializer{turns: map[string]*sync.Mutex{}}
}

func (serializer *localSerializer) Do(ctx context.Context, uuid string, fn func(context.Context) error) error {
	turn := serializer.turnFor(uuid)
	turn.Lock()
	defer turn.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(ctx)
}

// turnFor returns one VM's turn, creating it on first use. Turns are created and
// never removed: one mutex per UUID this Server has heard of, against a removal
// that would have to prove no goroutine is about to take the lock it is
// deleting.
func (serializer *localSerializer) turnFor(uuid string) *sync.Mutex {
	serializer.mutex.Lock()
	defer serializer.mutex.Unlock()
	turn, found := serializer.turns[uuid]
	if !found {
		turn = &sync.Mutex{}
		serializer.turns[uuid] = turn
	}
	return turn
}
