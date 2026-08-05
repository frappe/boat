package main

import (
	"context"
	"io"

	"github.com/frappe/boat/internal/cert"
	"github.com/frappe/boat/internal/hostkeys"
	"github.com/frappe/boat/internal/mgmtfirewall"
	"github.com/frappe/boat/internal/migration"
	"github.com/frappe/boat/internal/netapply/vmnetwork"
	"github.com/frappe/boat/internal/park"
	"github.com/frappe/boat/internal/reset"
	"github.com/frappe/boat/internal/run"
)

// regenerateHostKeysVM gives a cloned VM its own SSH host keys — `boat
// regenerate-host-keys-vm`, the port of regenerate-host-keys-vm.py. No result
// line, as the Python has none.
func regenerateHostKeysVM(arguments []string, errorOutput io.Writer) int {
	flags := newTaskFlags("regenerate-host-keys-vm", errorOutput)
	uuid := flags.requiredText("virtual-machine-name")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	_, err := hostkeys.RegenerateHostKeysVM(
		context.Background(), run.NewRunner(errorOutput), hostkeys.RegenerateHostKeysParams{VirtualMachine: *uuid},
	)
	if err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}

// issueCert obtains the region's wildcard certificate over a DNS-01 challenge —
// `boat issue-cert`, the port of issue-cert.py.
func issueCert(arguments []string, output io.Writer, errorOutput io.Writer) int {
	flags := newTaskFlags("issue-cert", errorOutput)
	domain := flags.requiredText("domain")
	directoryURL := flags.requiredText("acme-directory-url")
	accountEmail := flags.requiredText("account-email")
	authenticator := flags.requiredText("dns-authenticator")
	certbotArgs := flags.list("certbot-arg")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	result, err := cert.IssueCert(context.Background(), run.NewRunner(errorOutput), cert.IssueCertParams{
		Domain:           *domain,
		ACMEDirectoryURL: *directoryURL,
		AccountEmail:     *accountEmail,
		DNSAuthenticator: *authenticator,
		CertbotArgs:      *certbotArgs,
	})
	if err != nil {
		return reportError(errorOutput, err)
	}
	return emit(output, errorOutput, map[string]any{
		"fullchain_path": result.FullchainPath,
		"privkey_path":   result.PrivkeyPath,
		"not_before":     result.NotBefore,
		"not_after":      result.NotAfter,
	})
}

// managementFirewall is the three-verb family that closes the host's public
// face and opens it again — `boat mgmt-firewall-apply|confirm|revert`, the port
// of mgmt-firewall-apply.py and its two siblings.
//
// One entry point for three verbs because they are one idea with one argument
// shape: apply arms an auto-revert, confirm cancels it, revert is the timer
// firing early. Splitting them across three near-identical functions would put
// the shared flag set in three places, which is where two of them drift.
func managementFirewall(action string, arguments []string, output io.Writer, errorOutput io.Writer) int {
	flags := newTaskFlags("mgmt-firewall-"+action, errorOutput)
	// The defaults are the dataclass's: the wg handshake port, discovery of the
	// public interface from the default route, and a three-minute window.
	port := flags.number("wg-port", 51820)
	publicInterface := flags.text("public-interface", "")
	revertSeconds := flags.number("revert-seconds", 180)
	allowPorts := flags.list("public-allow-port")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	runner := run.NewRunner(errorOutput)
	ctx := context.Background()
	switch action {
	case "apply":
		result, err := mgmtfirewall.Apply(ctx, runner, mgmtfirewall.ApplyParams{
			WGPort:           *port,
			PublicInterface:  *publicInterface,
			RevertSeconds:    *revertSeconds,
			PublicAllowPorts: *allowPorts,
		})
		if err != nil {
			return reportError(errorOutput, err)
		}
		return emit(output, errorOutput, map[string]any{
			"public_interface":   result.PublicInterface,
			"wg_port":            result.WGPort,
			"revert_seconds":     result.RevertSeconds,
			"public_allow_ports": ports(result.PublicAllowPorts),
		})
	case "confirm":
		result, err := mgmtfirewall.Confirm(ctx, runner, mgmtfirewall.ConfirmParams{
			WGPort:           *port,
			PublicInterface:  *publicInterface,
			PublicAllowPorts: *allowPorts,
		})
		if err != nil {
			return reportError(errorOutput, err)
		}
		return emit(output, errorOutput, map[string]any{
			"confirmed":        result.Confirmed,
			"public_interface": result.PublicInterface,
		})
	case "revert":
		result, err := mgmtfirewall.Revert(ctx, runner, mgmtfirewall.RevertParams{})
		if err != nil {
			return reportError(errorOutput, err)
		}
		return emit(output, errorOutput, map[string]any{"reverted": result.Reverted})
	}
	return usage(errorOutput)
}

// resetServer sweeps every VM and its host state off the machine — `boat
// reset-server`, the port of reset-server.py. No result line, as the Python has
// none; the sweep's detail goes to the trace.
//
// The network teardown is handed in rather than imported inside internal/reset,
// which is what keeps that package free of the whole vmnetwork dependency for
// the one call it makes.
func resetServer(arguments []string, errorOutput io.Writer) int {
	flags := newTaskFlags("reset-server", errorOutput)
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	runner := run.NewRunner(errorOutput)
	networkDown := func(ctx context.Context, uuid string) error { return vmnetwork.Down(ctx, runner, uuid) }
	if _, err := reset.ResetServer(context.Background(), runner, reset.ResetParams{}, networkDown); err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}

// pollVMTraffic answers whether each of this host's idle-eligible VMs has moved
// any traffic since the last poll — `boat poll-vm-traffic`, the port of
// poll-vm-traffic.py.
//
// The delta is computed here rather than by the controller, so Atlas only ever
// sees a bool: the raw byte totals are host-local and ephemeral, and a counter in
// the database would be a number nobody could trust across a chain flush.
func pollVMTraffic(arguments []string, output io.Writer, errorOutput io.Writer) int {
	flags := newTaskFlags("poll-vm-traffic", errorOutput)
	virtualMachinesJSON := flags.requiredText("vms-json")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	targets, err := park.ParseTrafficTargets(*virtualMachinesJSON)
	if err != nil {
		return reportError(errorOutput, err)
	}
	active, err := park.PollTraffic(context.Background(), run.NewRunner(errorOutput), targets)
	if err != nil {
		return reportError(errorOutput, err)
	}
	return emit(output, errorOutput, map[string]any{"counters": trafficCounters(active)})
}

// probeWokenVMs answers which of the given Sleeping VMs this host has already
// woken — `boat probe-woken-vms`, the port of probe-woken-vms.py. Read-only: it
// changes nothing, and the host is simply the authority for a wake having
// happened, because only it saw the packet.
func probeWokenVMs(arguments []string, output io.Writer, errorOutput io.Writer) int {
	flags := newTaskFlags("probe-woken-vms", errorOutput)
	virtualMachinesJSON := flags.requiredText("vms-json")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	uuids, err := park.ParseUUIDs(*virtualMachinesJSON)
	if err != nil {
		return reportError(errorOutput, err)
	}
	woken, err := park.Woken(context.Background(), run.NewRunner(errorOutput), uuids)
	if err != nil {
		return reportError(errorOutput, err)
	}
	return emit(output, errorOutput, map[string]any{"woken": woken})
}

// exportCleanupSource stops a base-image export's NBD servers and drops its
// staged tar — `boat export-cleanup-source`, the port of
// export-cleanup-source.py. No result line, as the Python has none.
func exportCleanupSource(arguments []string, errorOutput io.Writer) int {
	flags := newTaskFlags("export-cleanup-source", errorOutput)
	imageName := flags.requiredText("image-name")
	port := flags.number("nbd-port", 0)
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	err := migration.ExportCleanupSource(
		context.Background(), run.NewRunner(errorOutput),
		migration.ExportCleanupSourceParams{ImageName: *imageName, NBDPort: *port},
	)
	if err != nil {
		return reportError(errorOutput, err)
	}
	return exitSuccess
}

// trafficCounters renders the poll's answer as the nested shape the controller's
// result field holds: {"<uuid>": {"active": bool}}. The nesting is the Python's
// and is kept even though there is one key today, because `cls(**payload)` reads
// the whole dict — a flattened bool would reach the controller as a TypeError on
// the first VM.
func trafficCounters(active map[string]bool) map[string]map[string]bool {
	counters := map[string]map[string]bool{}
	for uuid, moved := range active {
		counters[uuid] = map[string]bool{"active": moved}
	}
	return counters
}

// ports keeps an empty list out of the result as `[]` rather than `null`: the
// controller's field is a list, and JSON null reaches it as None.
func ports(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
