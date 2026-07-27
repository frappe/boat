// Package fcattach finds a Firecracker that is already running for a UUID and
// re-attaches to it, instead of restarting it.
//
// Re-attach, never restart, is the load-bearing capability of the whole
// Atlas→Boat split. Boat is a daemon that gets upgraded on a host full of live
// VMs; a Boat that relaunched Firecracker on its own startup would kill every
// guest on the host every time it was updated, and self-update is hard-gated on
// that not happening. This is libvirtd's model: the VMs are not the daemon's
// children, they are processes it re-discovers.
//
// Liveness is confirmed by TALKING TO THE SOCKET. Every VM's API socket path is
// a pure function of its UUID (paths.APISocket), so the socket is the one
// per-UUID rendezvous the launcher and Boat both derive with no shared state,
// and something answering HTTP on it is proof that a Firecracker is alive behind
// it. Two alternatives were rejected outright:
//
//   - Pattern-matching a process table (`pgrep firecracker`, argv scraping) is
//     racy against a pid that was recycled, spoofable by anything that can pick
//     its own argv, and it breaks the moment the launcher's command line changes
//     — which it does, per work order.
//   - Testing that the socket FILE exists is not liveness at all. A unix socket
//     inode outlives the process that bound it, so a Firecracker that segfaulted
//     leaves a socket sitting there that stat is perfectly happy with.
//
// What a "not found" answer means, and what it does not: it means no live
// Firecracker answered on this host, right now. It does NOT mean the VM is dead,
// destroyed, or safe to re-provision — a stopped VM, a sleeping VM and a VM
// mid-launch all look exactly like this. Everything upstream depends on that
// distinction, which is why an error here is reported as an error and never
// folded into "not found".
package fcattach

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
)

// Process is a live jailed Firecracker this host is already running.
type Process struct {
	UUID string
	// Pid is best effort and is 0 when it could not be determined. The socket,
	// not this number, is the authority on liveness: a Process with Pid 0 was
	// still proven alive, and a pid alone would prove nothing.
	Pid       int
	APISocket string
}

// commands is everything this package does to the host, and the only seam it
// has. One implementation outside tests, *run.Runner, and there is never a
// second.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
}

var _ commands = (*run.Runner)(nil)

// socketFiles is the slice of the path layout this package addresses a VM
// through: the absolute socket for stat, and the directory/name pair for the
// connect. Naming it here keeps the dependency on internal/paths to one
// function, so a test states the layout as literal strings.
type socketFiles struct {
	socket    string
	directory string
	name      string
}

func filesFor(uuid string) socketFiles {
	virtualMachine := paths.ForVirtualMachine(uuid)
	return socketFiles{
		socket:    virtualMachine.APISocket(),
		directory: virtualMachine.APISocketDirectory(),
		name:      virtualMachine.APISocketName(),
	}
}

// instanceInfoURL is Firecracker's GET / (InstanceInfo). curl wants a URL even
// when the transport is a unix socket, and the host part is ignored.
const instanceInfoURL = "http://localhost/"

// curlCouldNotConnect is curl(1)'s exit code for "failed to connect": over a
// unix socket that is ECONNREFUSED, i.e. the socket file is there and nobody is
// listening on it. It is the one non-zero exit that is an answer rather than a
// fault, and separating it from every other non-zero exit is what keeps "the VM
// is not running" apart from "I could not tell".
const curlCouldNotConnect = 7

// Find locates a live Firecracker for uuid through its deterministic per-UUID
// API socket.
//
// Three outcomes, and callers must keep them apart:
//
//	(process, true,  nil) — alive; attach to it, do not start anything.
//	(zero,    false, nil) — nothing live answered. NOT a claim that the VM is dead.
//	(zero,    false, err) — could not determine. Never read as either of the above.
func Find(ctx context.Context, runner *run.Runner, uuid string) (Process, bool, error) {
	return find(ctx, runner, filesFor(uuid), uuid)
}

// find is Find with the host seam passed in, so the whole package unit-tests
// with no Firecracker, no jail and no root.
func find(ctx context.Context, commands commands, files socketFiles, uuid string) (Process, bool, error) {
	// No socket at all is the ordinary answer for every stopped and sleeping VM
	// on the host, so it is settled with one cheap `test` before any curl. `-S`
	// and not `-f`: the socket is a socket, and `test -f` is false for one.
	if !commands.OK(ctx, "sudo test -S {}", files.socket) {
		return Process{}, false, nil
	}
	live, err := answering(ctx, commands, files)
	if err != nil || !live {
		return Process{}, false, err
	}
	return Process{UUID: uuid, Pid: pidOf(ctx, commands, files), APISocket: files.socket}, true, nil
}

// answering reports whether something on the far end of the socket speaks HTTP.
//
// The whole line runs under one `sudo sh -c` for the same reason
// run.FirecrackerAPI does: the absolute socket path is longer than AF_UNIX's
// 108-byte sun_path allows, so the connect has to cd into the socket's directory
// and address it by its short relative name — and that directory is 0700-owned
// by the per-VM uid, so the cd must be root. cd and curl are one shell line by
// construction. Like that call, this template is byte-coupled to a line in
// sudoers.d/boat.
//
// Deliberately NOT --fail, which is the one flag run.FirecrackerAPI wants and
// this probe must not have. A state change Firecracker refuses should fail the
// operation; a liveness probe asks a strictly weaker question, and ANY HTTP
// response — 200, 404 from a Firecracker whose API surface moved — proves a live
// process. Treating a 404 as "not running" would be a false negative, and false
// negatives are the dangerous direction here: they are how a controller decides
// a live VM is down.
func answering(ctx context.Context, commands commands, files socketFiles) (bool, error) {
	// --max-time bounds a Firecracker that accepted the connection and then never
	// answered. Adoption calls this once per VM on a host with dozens, and one
	// wedged guest must not hold the scan open forever.
	probe := "cd {} && curl --silent --show-error --max-time 2 --unix-socket {} {}"
	rendered, err := run.Substitute(probe, files.directory, files.name, instanceInfoURL)
	if err != nil {
		return false, err
	}
	if _, err := commands.Run(ctx, "sudo sh -c {}", rendered); err != nil {
		return false, classify(err, files)
	}
	return true, nil
}

// classify separates the stale socket from the broken host. A refused connect is
// data — the process that bound this socket is gone — and comes back as "not
// live" with no error. Anything else (a denied sudo, a missing curl, a probe
// that timed out, a directory that vanished mid-scan) is a failure to observe,
// and observing nothing is not the same as observing nothing running.
func classify(err error, files socketFiles) error {
	var commandError *run.CommandError
	if errors.As(err, &commandError) && commandError.ExitCode == curlCouldNotConnect {
		return nil
	}
	return fmt.Errorf("probing the Firecracker API socket %s: %w", files.socket, err)
}

// pidOf asks which process holds the socket that was just proven live, so the
// pid comes from the same authority the liveness claim does — rather than from
// the systemd unit's MainPID, which answers a different question ("what is this
// unit supervising") and would disagree exactly when it matters.
//
// Best effort by design: a Process with Pid 0 is still a Process that answered
// on its socket, and the backstop is that nothing decides anything from this
// number. It is a diagnostic that lets an operator strace the right process.
func pidOf(ctx context.Context, commands commands, files socketFiles) int {
	// sudo because -p only reveals another user's process to root, and the jailed
	// Firecracker runs as the per-VM uid. ss is iproute2, which every host
	// already carries for the networking layer; lsof and fuser are not installed.
	output, err := commands.Run(ctx, "sudo ss --no-header --unix --listening --processes src {}", files.socket)
	if err != nil {
		return 0
	}
	return parsePid(output)
}

// parsePid pulls the pid out of ss's `users:(("firecracker",pid=15843,fd=8))`
// column. Anything else in the line is ignored: the socket path was the filter,
// so the first pid ss prints is the holder of that socket.
func parsePid(output string) int {
	_, rest, found := strings.Cut(output, "pid=")
	if !found {
		return 0
	}
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	pid, err := strconv.Atoi(rest[:digits])
	if err != nil {
		return 0
	}
	return pid
}
