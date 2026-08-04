// Command boat is every host-side Boat service and the operator's client, in
// one binary — the busybox model. One build artifact per host means a host can
// never run a daemon of one version beside a helper of another, which is the
// whole reason version drift can be reported as a single fact.
//
// `boat daemon` is the API. Every other verb is a client of that same API over
// the local socket, so a break-glass command has no powers the API lacks and no
// second path into the host to keep in step.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/frappe/boat/internal/version"
)

const (
	exitSuccess = 0
	exitFailure = 1
	exitUsage   = 2
)

const usageText = `boat — the per-host VM daemon and its client

usage:
  boat daemon [--listen ADDR] [--socket PATH] [--store PATH] [--token-file PATH]
  boat vm start <uuid>
  boat vm stop <uuid> [--graceful=false] [--stop-timeout-seconds N]
  boat vm pause|resume|sleep|wake|terminate|resize <uuid>
  boat vm rebuild <uuid> (--image NAME | --snapshot-device DEV)
                         --identity-file PATH [--data-snapshot-device DEV]
  boat vm ls
  boat vm show <uuid>
  boat host facts
  boat vm-network-up <uuid>
  boat vm-network-down <uuid>
  boat vm-disk-up <uuid>
  boat bootstrap
  boat networkd
  boat image-import <name> <rootfs-file> <disk-gb>
  boat vm-create-disk <uuid> <image-name>
  boat metrics
  boat version

The six verbs on one line take a UUID and nothing else: their arguments are the
desired state Atlas already asserted, which the host reads for itself. A resize
run here re-applies that asserted shape rather than taking new numbers.

Every verb but daemon is a client of the daemon's HTTP API over its local
socket (default /run/boat/boat.sock, override with BOAT_SOCKET).
`

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

func dispatch(arguments []string, output io.Writer, errorOutput io.Writer) int {
	if len(arguments) == 0 {
		return usage(errorOutput)
	}
	switch arguments[0] {
	case "daemon":
		return daemon(arguments[1:], errorOutput)
	case "vm":
		return virtualMachineCommand(arguments[1:], output, errorOutput)
	case "host":
		return hostCommand(arguments[1:], output, errorOutput)
	// The firecracker-vm@ hooks, run directly on the host rather than through the
	// daemon (see network.go). Kebab-named to match the Python scripts they replace.
	case "vm-network-up":
		return vmNetworkUp(arguments[1:], errorOutput)
	case "vm-network-down":
		return vmNetworkDown(arguments[1:], errorOutput)
	case "vm-disk-up":
		return vmDiskUp(arguments[1:], errorOutput)
	case "bootstrap":
		// Brings THIS host to VM-ready — one command, boat driving every step.
		return bootstrapCommand(arguments[1:], errorOutput)
	case "networkd":
		// The ANCP wg-mesh gossip control plane (spec/31, WO-5). A long-running
		// daemon in its own unit, privileged for wg/ip — not a client of the API.
		return networkdCommand(arguments[1:], errorOutput)
	case "image-import":
		return imageImport(arguments[1:], errorOutput)
	case "vm-create-disk":
		return vmCreateDisk(arguments[1:], errorOutput)
	case "metrics":
		return metricsCommand(arguments[1:], output, errorOutput)
	case "update-apply":
		// The detached half of a self-update, spawned by POST /v1/update into its
		// own cgroup so the daemon restart cannot SIGTERM it mid-swap (see update.go).
		return updateApply(arguments[1:], errorOutput)
	case "version":
		// The binary's own identity, which is a local fact — the daemon's is on
		// /health, and the two differing is exactly what an operator wants to see.
		fmt.Fprintln(output, version.Version)
		return exitSuccess
	}
	return usage(errorOutput)
}

func virtualMachineCommand(arguments []string, output io.Writer, errorOutput io.Writer) int {
	if len(arguments) == 0 {
		return usage(errorOutput)
	}
	client := newDaemonClient(socketPath())
	switch arguments[0] {
	case "start":
		return startVirtualMachine(arguments[1:], client, output, errorOutput)
	case "stop":
		return stopVirtualMachine(arguments[1:], client, output, errorOutput)
	// The verb name is also the path segment, which is not a coincidence to be
	// tidied away: the CLI is a client of the documented API and nothing else, so
	// a verb reachable here that the API does not serve is a 404 rather than a
	// second way into the host.
	case "pause", "resume", "sleep", "wake", "terminate", "resize":
		return plainVerb(arguments[0], arguments[1:], client, output, errorOutput)
	case "rebuild":
		return rebuildVirtualMachine(arguments[1:], client, output, errorOutput)
	case "ls":
		return listVirtualMachines(client, output, errorOutput)
	case "show":
		return showVirtualMachine(arguments[1:], client, output, errorOutput)
	}
	return usage(errorOutput)
}

func hostCommand(arguments []string, output io.Writer, errorOutput io.Writer) int {
	if len(arguments) == 0 || arguments[0] != "facts" {
		return usage(errorOutput)
	}
	return showHostFacts(newDaemonClient(socketPath()), output, errorOutput)
}

func usage(errorOutput io.Writer) int {
	fmt.Fprint(errorOutput, usageText)
	return exitUsage
}

// reportError states a failure on stderr in the daemon's own words, so the
// operator reads the sentence the API produced rather than a paraphrase.
func reportError(errorOutput io.Writer, err error) int {
	fmt.Fprintln(errorOutput, err)
	return exitFailure
}
