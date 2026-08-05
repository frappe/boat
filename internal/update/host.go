package update

// host.go is the concrete daemon-side Host (apply.go's interface): the three
// effects Apply cannot express as a subprocess and must ask the running host to
// perform — quiesce, restart-and-reattach, health-check — plus the abort-path
// Resume. apply.go decides the ORDER; this file is the MECHANISM, and every piece
// of host it touches is an injected seam so the sequencing tests run with no
// systemd, no socket and no Firecracker, exactly as apply_test.go's fakeHost does.
//
// # Why the seams, one per effect
//
//   - Quiesce/Resume go through Quiescer, because §5 step 3's "refuse new
//     operations and checkpoint in-flight ones into the journal" is a host-wide
//     pause that DOES NOT EXIST as a callable API today. internal/reconcile has
//     per-VM serialization (its Do) and a whole-reconciler lifetime cancel, and
//     internal/journal has Record, but nothing composes them into "stop admitting
//     verbs, drain what is running, checkpoint it." Building that lives with the
//     daemon (it spans the API admission layer, the reconciler and the journal in
//     one transaction-shaped dance), so this package names only the shape it calls
//     and the daemon wires the real one — the WO split apply.go already describes.
//   - RestartAndReattach and Healthy go through a run seam and an HTTP seam,
//     because they ARE concrete here: a `systemctl restart` per unit, and a GET
//     /health round-trip against the just-swapped binary.
//
// The heavy commenting is boat's house style (see release.go): the WHY of each
// step, because the failure mode a wrong step causes is a live host under guests.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/frappe/boat/internal/run"
)

// Quiescer is THE seam the parent must implement on the real daemon. It is §5
// step 3 (and its abort-path undo) as two calls, defined here because no host-wide
// quiesce API exists in internal/reconcile or internal/journal yet — see this
// file's package note.
//
// Quiesce must, on the real daemon: stop admitting new verbs at the API layer,
// let the operations already in flight reach a journal checkpoint (the boat.service
// unit already describes this exact behaviour for its own SIGTERM shutdown —
// "lets in-flight operations reach a journal checkpoint"), and return only once
// the host is idle enough that a restart replays cleanly rather than losing work.
// Resume is the strict inverse and is called ONLY on an abort before the swap
// (apply.go's resumeAfter): once RestartAndReattach has run, a fresh daemon is
// serving and there is nothing quiesced to resume.
//
// It is deliberately minimal — two methods, no arguments beyond the context — so
// the daemon's later wiring is free to compose reconcile + journal + the API
// admission gate however it must, and this package never grows an opinion about
// their internals. Do NOT add the pause logic to internal/reconcile or
// internal/journal from here; that is the parent's wiring, outside this package.
type Quiescer interface {
	Quiesce(ctx context.Context) error
	Resume(ctx context.Context) error
}

// Runner is the subprocess seam RestartAndReattach and Healthy need — the one
// run.Runner method they use, narrowed so a test records the exact `systemctl`
// lines with no host. It is the same shape install.go's `commands` narrows to,
// kept separate because a Host has no InstallFile to give and a fake should not
// have to invent one. *run.Runner is the only implementation outside tests.
type Runner interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
}

var _ Runner = (*run.Runner)(nil)

// HTTPDoer is the http.Client method Healthy needs, narrowed so a test answers
// /health with a canned response and no socket under it. *http.Client satisfies
// it; production hands NewHost the UnixSocketClient below.
type HTTPDoer interface {
	Do(request *http.Request) (*http.Response, error)
}

var _ HTTPDoer = (*http.Client)(nil)

const (
	// The long-running daemon units RestartAndReattach swaps onto the new binary.
	// Both are the SAME /usr/local/bin/boat under a different subcommand (see
	// boat.service / boat-networkd.service), so one binary swap plus these two
	// restarts re-points the whole host at the new build.
	//
	// The per-VM firecracker-vm@ units are deliberately NOT here: restarting one
	// restarts its Firecracker and kills the guest, which is the one thing §3.3 and
	// this whole update forbid. The units list must never include them.
	daemonUnit   = "boat.service"          // `boat daemon`: the API + reconciler
	networkdUnit = "boat-networkd.service" // `boat networkd`: the wg-mesh control plane

	// healthURL is the local /health probe's target. The host part is a label only:
	// UnixSocketClient dials the unix socket regardless, so this names the path
	// (/health, which api/handler.go serves bare and under /v1) and nothing routes
	// on the authority. It mirrors api/openapi.yaml's `http://boat/v1` server URL.
	healthURL = "http://boat/health"

	// healthTimeout backstops the /health round-trip and the socket dial, so a new
	// binary that came up but wedged its listener fails the health check within a
	// bounded window rather than hanging Apply — and therefore rolls back — instead
	// of leaving the host mid-swap forever. The caller's context still cuts it
	// shorter when it wants to.
	healthTimeout = 5 * time.Second
)

// DefaultUnits is the production restart set and ORDER: the wg-mesh control plane
// first, the API+reconciler daemon last.
//
// Last is deliberate for boat.service: it is the unit Healthy probes (it serves
// /health) and the one that re-adopts Firecracker on startup, so ending the
// sequence with it means the component the health check immediately interrogates
// is the freshly-restarted one, and the reconciler that drives VMs is the last
// thing to come back — a daemon being replaced should stop driving units before
// its successor starts (internal/reconcile/run.go says this of its own shutdown).
func DefaultUnits() []string { return []string{networkdUnit, daemonUnit} }

// DaemonHost is the concrete Host apply.go orchestrates. Its collaborators are all
// injected: the Quiescer the daemon implements, the run seam, the /health client
// and the ordered unit set. It holds no state between calls — every method reads
// the host afresh, because the host is the state.
type DaemonHost struct {
	quiescer Quiescer
	runner   Runner
	health   HTTPDoer
	units    []string
}

var _ Host = (*DaemonHost)(nil)

// NewHost wires a DaemonHost from its injected dependencies. An empty units slice
// is substituted with DefaultUnits rather than tolerated: a Host that restarted
// NOTHING would report a healthy update while the old binary kept serving, which
// is the silent no-op §5 exists to prevent.
//
// Production construction (the parent writes this): quiescer is the daemon's own
// implementation of Quiescer over reconcile + journal; runner is a *run.Runner;
// health is UnixSocketClient("/run/boat/boat.sock"); units is DefaultUnits().
func NewHost(quiescer Quiescer, runner Runner, health HTTPDoer, units []string) *DaemonHost {
	if len(units) == 0 {
		units = DefaultUnits()
	}
	return &DaemonHost{quiescer: quiescer, runner: runner, health: health, units: units}
}

// UnixSocketClient is the production HTTPDoer: an http.Client that ignores the
// request's authority and dials the daemon's unix socket. It is a helper rather
// than logic inside NewHost so a test can inject a fake Doer instead, and so the
// one socket path lives with the caller that knows it (the daemon passes
// /run/boat/boat.sock, matching boat.service's --socket).
func UnixSocketClient(socketPath string) *http.Client {
	return &http.Client{
		Timeout: healthTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

// Quiesce is §5 step 3, delegated to the injected Quiescer. There is nothing to do
// here but call it: the mechanism (refuse new verbs, checkpoint in-flight ones)
// belongs to the daemon, and this package is only the point in the sequence at
// which it happens — before the swap is made live by a restart.
func (host *DaemonHost) Quiesce(ctx context.Context) error {
	if err := host.quiescer.Quiesce(ctx); err != nil {
		return fmt.Errorf("quiesce the daemon: %w", err)
	}
	return nil
}

// Resume is the abort-path inverse of Quiesce, delegated likewise. apply.go calls
// it only when an update gives up BEFORE the restart; after the restart a fresh
// daemon is serving and there is nothing quiesced to resume.
func (host *DaemonHost) Resume(ctx context.Context) error {
	if err := host.quiescer.Resume(ctx); err != nil {
		return fmt.Errorf("resume the daemon: %w", err)
	}
	return nil
}

// RestartAndReattach is §5 step 4: restart the daemon units in order onto the
// binary Install swapped in. It is only the restart — there is no explicit
// re-attach — and that omission is the load-bearing part.
//
// # Why no re-attach step
//
// The restarted `boat daemon` re-attaches by STARTING. On startup it runs
// internal/adopt, which reconstructs what the host holds by reading it — VM
// directories, firecracker-vm@ units, netns, disks — and re-attaches to the
// Firecracker processes already running (adopt's package doc: "a Boat started on a
// live machine learns its VMs by reading the host instead of by asking a
// database"). Firecracker is NOT boat's child: boat.service is deliberately not
// ordered Before=firecracker-vm@.service and its comment states "Firecracker is
// not our child and keeps running." So restarting the daemon leaves every live
// guest running and the new process simply adopts them — §3.3's re-attach, for
// free, with no step of ours to get wrong.
//
// # Why sleeping VMs stay asleep across the restart
//
// A sleeping VM parks its firecracker-vm@ unit behind a ConditionPathExists=! on
// its sleep marker, so systemd never (re)starts it, and adopt "never mutates" —
// it reads the marker and reports the VM asleep, it does not wake it (mirrored by
// reconcile, whose sweep "converges power and leaves a sleeping VM asleep"). A
// daemon restart therefore neither wakes a parked guest nor disturbs its staged
// memory snapshot.
//
// # The one constraint on the CALLER
//
// This must be invoked from a process OUTSIDE boat.service's cgroup — the update
// orchestrator is a short-lived `boat update`-style process, not the serving
// daemon itself. `systemctl restart boat.service` stops the whole cgroup, so an
// orchestrator running inside it would be SIGTERM'd mid-restart and never reach
// Healthy or a rollback. Run from its own scope, the restart is synchronous
// (Type=exec holds until the new binary is exec'd) and returns to let Healthy
// probe. That placement is the parent's wiring; this method only issues the
// commands.
//
// Fail-fast: the first unit that will not restart aborts the rest and returns,
// which is what makes apply.go roll the whole host back to N-1 rather than leave
// it half-swapped.
func (host *DaemonHost) RestartAndReattach(ctx context.Context) error {
	for _, unit := range host.units {
		if _, err := host.runner.Run(ctx, "sudo systemctl restart {}", unit); err != nil {
			return fmt.Errorf("restart %s onto the new binary: %w", unit, err)
		}
	}
	return nil
}

// Healthy is §5 step 5: prove the just-swapped binary actually serves and its
// units are actually up. A non-nil error here is what apply.go rolls back on, so
// this must be a real round-trip, not a guess.
//
// Two independent checks, and both are needed:
//
//   - The /health round-trip talks to the process over the socket and so proves
//     the NEW binary is answering HTTP — the thing systemd's own "active" cannot
//     tell us, because a binary that exec'd and then wedged its listener is still
//     "active" to systemd for a moment. /health over /export because it is
//     unauthenticated (no token needed to prove liveness), served on the socket,
//     and the minimal proof the swap serves; a deeper /export check could layer on
//     top but liveness is the step-5 gate.
//   - `systemctl is-active` per restarted unit catches the other failure: a binary
//     that came up, answered once, then crash-looped (Restart=on-failure) or
//     exited. is-active exits non-zero for anything but a live unit, and the run
//     seam turns that non-zero exit into an error, so no output parsing is needed.
//
// The socket probe runs first: it is the primary "does the new code serve"
// signal, and a failure there is the more informative one to report.
func (host *DaemonHost) Healthy(ctx context.Context) error {
	if err := host.probeHealth(ctx); err != nil {
		return err
	}
	for _, unit := range host.units {
		// A read, so no sudo and no operation record — is-active is a status query
		// the service user may make of its own units.
		if _, err := host.runner.Run(ctx, "systemctl is-active {}", unit); err != nil {
			return fmt.Errorf("unit %s is not active after the swap: %w", unit, err)
		}
	}
	return nil
}

// probeHealth is the GET /health round-trip against the local socket. It bounds
// itself with healthTimeout so a wedged listener fails the check rather than
// hanging Apply, drains and closes the body so the connection can be reused, and
// treats anything but 200 as unhealthy.
func (host *DaemonHost) probeHealth(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return fmt.Errorf("build the /health request: %w", err)
	}
	response, err := host.health.Do(request)
	if err != nil {
		return fmt.Errorf("the swapped binary did not answer /health: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the swapped binary answered /health with status %d", response.StatusCode)
	}
	return nil
}
