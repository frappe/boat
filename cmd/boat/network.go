package main

import (
	"context"
	"io"

	"github.com/frappe/boat/internal/netapply/vmnetwork"
	"github.com/frappe/boat/internal/run"
)

// vmNetworkUp and vmNetworkDown are the firecracker-vm@ unit's ExecStartPre and
// ExecStopPost, run directly on the host rather than through the daemon. Unlike
// every other verb they are NOT clients of the API: a VM's host-side networking
// must exist before the jailer joins the namespace and be torn down after the
// unit stops, both synchronous to that unit's own lifecycle, so the hook runs the
// mechanic in-process. This is THE RULE at work — one binary backs every unit, and
// `boat vm-network-up %i` is the hook the Python `vm-network-up.py %i` becomes.
//
// The trace goes to stderr, which is where systemd folds a hook's output into the
// journal, so an operator reads the same `+ command` lines the Python emitted.
func vmNetworkUp(arguments []string, errorOutput io.Writer) int {
	return runNetworkHook(arguments, errorOutput, vmnetwork.Up)
}

func vmNetworkDown(arguments []string, errorOutput io.Writer) int {
	return runNetworkHook(arguments, errorOutput, vmnetwork.Down)
}

func runNetworkHook(
	arguments []string, errorOutput io.Writer, apply func(context.Context, *run.Runner, string) error,
) int {
	if len(arguments) != 1 {
		return usage(errorOutput)
	}
	runner := run.NewRunner(errorOutput)
	if err := apply(context.Background(), runner, arguments[0]); err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}
