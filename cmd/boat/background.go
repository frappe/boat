package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// wakeTrapInterval is how often the trap reads the wake counters.
//
// One second, because that is the client's own retransmit interval for a SYN: a
// sleeping VM has about that long to be listening before the connection attempt
// is given up on, and a slower poll spends the guest's whole restore budget
// doing nothing. It is the interval atlas-wake-trap.py polled at, for the same
// reason.
const wakeTrapInterval = time.Second

// background is the daemon's two resident loops — the reconciler and the wake
// trap — and the one cancellation that ends both.
//
// It holds a context, which a struct is normally not allowed to do. Here the
// context IS the value's subject: this type exists to own the lifetime of work
// that outlives the call that started it, and cancelling it is the whole of how
// a daemon being replaced by a new binary stops driving units before the new
// one starts.
type background struct {
	ctx     context.Context
	cancel  context.CancelFunc
	running sync.WaitGroup
}

func newBackground() *background {
	ctx, cancel := context.WithCancel(context.Background())
	return &background{ctx: ctx, cancel: cancel}
}

// runBackground starts the loops that drive this host, both of them reaching
// the host through the reconciler and neither of them around it.
func (parts *daemonParts) runBackground() *background {
	work := newBackground()
	work.run("reconciler", parts.reconciler.Run)
	work.run("wake trap", func(ctx context.Context) error {
		return parts.trap.Run(ctx, wakeTrapInterval)
	})
	return work
}

// run starts one loop.
//
// A loop that ends before it was asked to is reported and does not take the
// daemon down with it. The API is how this host is diagnosed and how Atlas
// re-asserts intent, so a Boat that answers with a dead reconciler is still
// worth more to an operator than one that exits: the fault is loud in the
// journal either way, and only one of the two can be fixed remotely.
func (work *background) run(name string, loop func(context.Context) error) {
	work.running.Add(1)
	go func() {
		defer work.running.Done()
		err := loop(work.ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("a background loop ended", "loop", name, "error", err)
			return
		}
		slog.Info("a background loop stopped", "loop", name)
	}()
}

// stopAndWait ends both loops and waits for them to return, so that nothing is
// still driving a unit or writing to the store when the caller closes it.
//
// The wait is bounded by ctx, which is the shutdown grace. A loop that outlasts
// it is reported and left running, because the alternative is a daemon that
// hangs past its unit's TimeoutStopSec and is SIGKILLed mid-transaction instead
// of exiting on its own terms.
func (work *background) stopAndWait(ctx context.Context) error {
	work.cancel()
	stopped := make(chan struct{})
	go func() {
		work.running.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		return errors.New("the reconciler and the wake trap did not stop within the shutdown grace")
	}
}
