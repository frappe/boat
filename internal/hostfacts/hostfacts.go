// Package hostfacts reads what this host is, right now.
//
// Live facts, never a bootstrap snapshot. Atlas's placement divides by these
// numbers on every provision, and capacity that drifts silently is capacity that
// overcommits: a host that grew a disk, lost a stick of RAM, or filled its thin
// pool since bootstrap is a host whose stamped totals are a lie the controller
// has no way to notice. Reading them on demand is the whole point — it is what
// Atlas's Refresh Capacity button asks for, made continuous.
//
// Ported from Atlas's scripts/lib/atlas/hostfacts.py (the capacity totals),
// scripts/server-facts.py (the Task that re-reads them) and
// scripts/lib/atlas/hostinfo.py (the host signature), comments and all.
//
// Every read goes through internal/run rather than through os.ReadFile, /proc
// included. run is the only package in Boat that touches the host, which is what
// keeps this one — the one whose parsing bites on real hosts — unit-testable
// with no host, no LVM stack and no root.
package hostfacts

import (
	"context"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/version"
)

// commands is everything this package does to the host, and the only seam it
// has. Outside tests there is one implementation, *run.Runner, and there is
// never a second. RunUnchecked is here for exactly one command; see
// readFirecrackerVersion.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
}

var _ commands = (*run.Runner)(nil)

// Read measures the live host.
//
// It fails on the first thing it cannot read rather than returning a HostFacts
// with a hole in it. Every field is either a capacity number placement packs
// against or a component of the signature a warm restore is gated on, and a zero
// in either is worse than no answer: a zero reads as "this host has no room" or
// as "this snapshot matches", and both are decisions taken on a fact nobody
// actually has.
func Read(ctx context.Context, runner *run.Runner) (model.HostFacts, error) {
	return read(ctx, runner)
}

// read is Read with the host seam passed in.
func read(ctx context.Context, commands commands) (model.HostFacts, error) {
	facts, err := identify(ctx, commands)
	if err != nil {
		return model.HostFacts{}, err
	}
	if err := addCapacity(ctx, commands, &facts); err != nil {
		return model.HostFacts{}, err
	}
	if err := addSignature(ctx, commands, &facts); err != nil {
		return model.HostFacts{}, err
	}
	return facts, nil
}

// identify reads who this host is. The Boat version needs no host at all: every
// systemd unit on the host runs this same binary, so the running version is a
// fact about the host that this process already knows about itself.
func identify(ctx context.Context, commands commands) (model.HostFacts, error) {
	facts := model.HostFacts{BoatVersion: version.Version}
	hostname, err := line(ctx, commands, "uname -n")
	if err != nil {
		return facts, fmt.Errorf("reading the hostname: %w", err)
	}
	// uname -r is platform.release(), which is the exact string the host
	// signature is composed from — not the distribution's kernel package version.
	kernel, err := line(ctx, commands, "uname -r")
	if err != nil {
		return facts, fmt.Errorf("reading the kernel version: %w", err)
	}
	facts.Hostname, facts.KernelVersion = hostname, kernel
	return facts, nil
}

// line runs a command whose whole answer is one short line and trims it. lvs and
// nproc both pad their output, and every parse below wants the bare token.
func line(ctx context.Context, commands commands, template string, parameters ...any) (string, error) {
	output, err := commands.Run(ctx, template, parameters...)
	return strings.TrimSpace(output), err
}
