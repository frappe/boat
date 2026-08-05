package main

import (
	"context"
	"io"

	"github.com/frappe/boat/internal/cert"
	"github.com/frappe/boat/internal/hostkeys"
	"github.com/frappe/boat/internal/mgmtfirewall"
	"github.com/frappe/boat/internal/netapply/vmnetwork"
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

// ports keeps an empty list out of the result as `[]` rather than `null`: the
// controller's field is a list, and JSON null reaches it as None.
func ports(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
