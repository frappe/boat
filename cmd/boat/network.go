package main

import (
	"context"
	"io"

	"github.com/frappe/boat/internal/netapply/vmnetwork"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/vmdisk"
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

// vmDiskUp is the unit's disk ExecStartPre — re-activate the VM's thin snapshot LV
// and re-expose its block node in the jail. A host hook like the network ones, run
// in-process (not through the daemon), because it must complete before the jailer's
// ExecStart opens rootfs.ext4.
func vmDiskUp(arguments []string, errorOutput io.Writer) int {
	return runNetworkHook(arguments, errorOutput, vmdisk.Up)
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

// firewallApply restricts or reopens one VM's public ingress — `boat
// firewall-apply`, the port of firewall-apply.py. No result line, as the Python
// has none.
//
// A Task rather than a hook, unlike the two above: it is dispatched by the
// Firewall doctype's apply/clear, and the sidecar it writes is what makes the
// change survive the next cold boot through vm-network-up.
func firewallApply(arguments []string, errorOutput io.Writer) int {
	flags := newTaskFlags("firewall-apply", errorOutput)
	uuid := flags.requiredText("virtual-machine-name")
	action := flags.text("action", "apply")
	// Repeatable: --rule tcp/443 --rule udp/1194. Absent on a deny-all-public
	// firewall, which is a real configuration and not a missing argument.
	rules := flags.list("rule")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	err := vmnetwork.Firewall(context.Background(), run.NewRunner(errorOutput), vmnetwork.FirewallParams{
		VirtualMachine: *uuid,
		Action:         *action,
		Rules:          *rules,
	})
	if err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}

// vmTunnel brings one VM's WireGuard tunnel up or down — `boat vm-tunnel`, the
// port of vm-tunnel.py. It reports the host's PUBLIC key; the private half never
// leaves the host, so `down` reports it empty.
func vmTunnel(arguments []string, output io.Writer, errorOutput io.Writer) int {
	flags := newTaskFlags("vm-tunnel", errorOutput)
	tunnelName := flags.requiredText("tunnel-name")
	uuid := flags.requiredText("virtual-machine-name")
	interfaceName := flags.requiredText("interface")
	action := flags.text("action", "up")
	// The up-only parameters, defaulted the way the dataclass defaults them: a
	// down is dispatched without any of them and works from the interface alone.
	listenPort := flags.number("listen-port", 0)
	clientPublicKey := flags.text("client-public-key", "")
	clientAddress := flags.text("client-address", "")
	hostAddress := flags.text("host-address", "")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	result, err := vmnetwork.Tunnel(context.Background(), run.NewRunner(errorOutput), vmnetwork.TunnelParams{
		TunnelName:      *tunnelName,
		VirtualMachine:  *uuid,
		Interface:       *interfaceName,
		Action:          *action,
		ListenPort:      *listenPort,
		ClientPublicKey: *clientPublicKey,
		ClientAddress:   *clientAddress,
		HostAddress:     *hostAddress,
	})
	if err != nil {
		return reportError(errorOutput, err)
	}
	return emit(output, errorOutput, map[string]any{"server_public_key": result.ServerPublicKey})
}
