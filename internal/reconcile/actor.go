package reconcile

import (
	"context"
	"sync"
	"time"
)

// trigger says why a pass was asked for. It exists for exactly one decision —
// whether a sleeping VM is resumed — and the order matters: a request outranks a
// sweep, so a wake that lands on a pending sweep is upgraded and never the other
// way round.
type trigger int

const (
	// triggerSweep is the periodic pass, and the pass a resume runs. It converges
	// power and leaves a sleeping VM asleep.
	triggerSweep trigger = iota
	// triggerRequest is a pass someone asked for by name: a wake trap's SYN, or a
	// verb that has just changed what this VM should be.
	triggerRequest
)

// actor is one VM's turn to be driven, plus the mailbox of passes waiting for it.
//
// The two locks have different jobs and are never held together. turn is the
// one-actor-per-VM rule itself and is held for as long as the work takes, which
// may be a whole boot. state guards the mailbox for a few instructions at a
// time, so posting a pass to a VM that is mid-boot does not wait for the boot.
type actor struct {
	uuid string
	turn sync.Mutex

	state    sync.Mutex
	pending  bool
	why      trigger
	serving  bool
	failures int
}

// request posts a pass and reports whether the caller has to start a goroutine
// to serve it.
//
// The whole mailbox is three fields under one lock because the interesting case
// is the race between a request arriving and a serving goroutine deciding it has
// nothing left to do. serving is only ever cleared under this same lock, so a
// request either finds it true — and is picked up by the loop that is still
// running — or finds it false and starts its own. There is no window in which a
// request is both not served and not starting a server.
func (actor *actor) request(why trigger) (serve bool) {
	actor.state.Lock()
	defer actor.state.Unlock()
	actor.pending = true
	if why > actor.why {
		actor.why = why
	}
	if actor.serving {
		return false
	}
	actor.serving = true
	return true
}

// take claims the next pending pass, or ends this goroutine's service when the
// mailbox is empty.
func (actor *actor) take() (trigger, bool) {
	actor.state.Lock()
	defer actor.state.Unlock()
	if !actor.pending {
		actor.serving = false
		return triggerSweep, false
	}
	actor.pending = false
	why := actor.why
	actor.why = triggerSweep
	return why, true
}

// backoffAfter records how the pass went and returns how long to wait before the
// next one. A success clears the history, so a VM that recovers is reconciled at
// full speed again rather than serving out the backoff its last failure earned.
func (actor *actor) backoffAfter(err error, policy backoff) time.Duration {
	actor.state.Lock()
	defer actor.state.Unlock()
	if err == nil {
		actor.failures = 0
		return 0
	}
	actor.failures++
	return policy.delay(actor.failures)
}

// serve runs passes for one VM until its mailbox is empty. It is the only thing
// that runs a pass, and there is at most one of it per VM, so a pass is
// serialized against another pass by construction and against a verb by the turn
// lock inside Do.
func (reconciler *Reconciler) serve(actor *actor) {
	for reconciler.lifetime.Err() == nil {
		why, serving := actor.take()
		if !serving {
			return
		}
		err := reconciler.pass(reconciler.lifetime, actor.uuid, why)
		// A pass cut short by the shutdown is not this VM's failure, and counting it
		// as one would have a daemon on its way out logging a backoff for every VM
		// it holds.
		if reconciler.lifetime.Err() != nil {
			return
		}
		reconciler.settle(actor, err)
	}
}

// pass runs one reconcile pass as this VM's actor, which is what makes a pass
// and a verb mutually exclusive.
func (reconciler *Reconciler) pass(ctx context.Context, uuid string, why trigger) error {
	return reconciler.Do(ctx, uuid, func(ctx context.Context) error {
		return reconciler.converge(ctx, uuid, why)
	})
}

// settle applies the backoff a failed pass earned.
//
// The wait is deliberately outside the turn lock — Do has already returned — so
// a VM serving out a five-minute backoff still accepts an operator's verb
// immediately. Backing off is about not spinning on the host, not about holding
// the VM hostage.
func (reconciler *Reconciler) settle(actor *actor, err error) {
	delay := actor.backoffAfter(err, reconciler.backoff)
	if delay == 0 {
		return
	}
	logger(actor.uuid).Warn("backing off after a failed reconcile pass", "delay", delay, "error", err)
	reconciler.wait(reconciler.lifetime, delay)
}

// backoff spaces the attempts after a pass fails.
type backoff struct {
	base time.Duration
	max  time.Duration
}

// delay doubles with each consecutive failure and stops at max.
//
// There is no jitter, and it is not an oversight: every wait here is against
// this host's own systemd, so the thundering herd jitter exists to prevent has
// no shared server to fall on. What matters instead is the cap, because a VM
// that is failing for a reason someone has since fixed has to be retried without
// waiting out an exponent that has run away.
func (backoff backoff) delay(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	delay := backoff.base
	for attempt := 1; attempt < failures && delay < backoff.max; attempt++ {
		delay *= 2
	}
	if delay > backoff.max {
		return backoff.max
	}
	return delay
}
