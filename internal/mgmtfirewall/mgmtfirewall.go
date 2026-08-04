// Package mgmtfirewall locks down an Atlas host's PUBLIC interface behind a
// default-deny nftables ruleset, with a confirm-or-auto-revert safety so a bad
// rule can never lock the operator — or Central — out (spec/21-tunnel.md).
//
// The lockdown drops all inbound on the public interface except the WireGuard UDP
// port (the one thing that lets Central dial in), loopback, established/related,
// ICMP, and an operator-configurable allow list. Every NON-public interface — wg0,
// loopback, a private NIC — stays wide open (policy accept), so Frappe and SSH over
// the tunnel keep working while the public side goes dark. This is a SEPARATE nft
// table (`inet atlas_mgmt`) that only hooks `input`, never `forward`, so locking
// down the management plane never touches a hosted VM's traffic.
//
// The three verbs are a confirm/revert pair around an armed timer:
//
//   - Apply loads the locked ruleset AND arms a systemd-run transient timer that
//     REVERTS (deletes the table, restoring open access) after N seconds unless a
//     Confirm cancels it first. The lockdown is live immediately, so a failed
//     handoff undoes itself rather than stranding the host unreachable.
//   - Confirm cancels the armed revert and persists the ruleset as the boot
//     default. It is called by Central OVER THE TUNNEL — arriving over wg0 proves
//     end-to-end reachability before the public side is made permanently dark.
//   - Revert is both the rollback path and what the armed timer's effect mirrors:
//     cancel the timer, delete the live table, and remove the persisted ruleset +
//     boot unit so a reboot does not re-lock.
//
// The pure ruleset builders (mgmtRuleset, loadableRuleset) are unit-testable with
// no host; only the three verbs touch it, through the one `commands` seam. Ported
// from scripts/mgmt-firewall-{apply,confirm,revert}.py and
// scripts/lib/atlas/mgmt_firewall.py.
package mgmtfirewall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/run"
)

const (
	// table is the management-plane nft table, separate from the data-plane
	// `inet atlas` table so a lockdown never touches VM forwarding.
	table = "atlas_mgmt"
	// revertUnit is the transient systemd unit name the armed auto-revert runs under.
	revertUnit = "atlas-firewall-revert"
	// stagingPath is where Apply stages the loadable ruleset before `nft -f`.
	stagingPath = "/run/atlas-mgmt.nft"
	// persistPath + persistUnit are how Confirm/Revert make the lockdown survive (or
	// not survive) a reboot: the boot unit (ordered Before=network-pre.target,
	// fail-closed) loads the persisted ruleset.
	persistPath = "/etc/atlas/mgmt-firewall.nft"
	persistUnit = "atlas-mgmt-firewall.service"
)

// commands is everything the three verbs do to the host, and the only seam they
// have. Outside tests there is one implementation, *run.Runner.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	InstallFile(ctx context.Context, content string, destination string, mode string) error
	InstallDirectory(ctx context.Context, destination string, mode string) error
}

var _ commands = (*run.Runner)(nil)

// --- pure builders (unit-testable, no host) ---------------------------------

// mgmtRuleset is the `table inet atlas_mgmt { … }` text. Only the public interface
// jumps to the drop chain; everything else rides the `policy accept`. The accept
// order matters: established/related first (so the host's own outbound gets
// replies), then the narrow inbound allowances, then drop.
func mgmtRuleset(publicInterface string, wgPort int, publicAllowPorts []string) string {
	allow := ""
	if len(publicAllowPorts) > 0 {
		allow = "\t\ttcp dport { " + strings.Join(publicAllowPorts, ", ") + " } accept\n"
	}
	return "table inet " + table + " {\n" +
		"\tchain input {\n" +
		"\t\ttype filter hook input priority filter; policy accept;\n" +
		"\t\tiifname \"" + publicInterface + "\" jump public_input\n" +
		"\t}\n" +
		"\tchain public_input {\n" +
		"\t\tct state established,related accept\n" +
		"\t\tct state invalid drop\n" +
		"\t\tmeta l4proto { icmp, icmpv6 } accept\n" +
		// UDP/wg_port carries BOTH WireGuard planes into this host: the Central control
		// tunnel and the host-mesh private fabric. Both listen on the same fixed port,
		// so this one accept covers both.
		fmt.Sprintf("\t\tudp dport %d accept\n", wgPort) +
		allow +
		"\t\tdrop\n" +
		"\t}\n" +
		"}\n"
}

// loadableRuleset is mgmtRuleset prefixed with the idempotent add-delete-add idiom,
// so `nft -f` replaces any existing atlas_mgmt table cleanly (an empty add makes the
// delete safe even on first apply).
func loadableRuleset(publicInterface string, wgPort int, publicAllowPorts []string) string {
	return "table inet " + table + " {}\ndelete table inet " + table + "\n" +
		mgmtRuleset(publicInterface, wgPort, publicAllowPorts)
}

// --- Apply ------------------------------------------------------------------

// ApplyParams is the lockdown to apply. The parent threads these from the API/CLI
// layer; the Python dataclass defaults it must supply are WGPort 51820 and
// RevertSeconds 180 (PublicInterface empty means discover from the default route,
// PublicAllowPorts empty means no extra public ports).
type ApplyParams struct {
	WGPort           int      // the one public inbound port (the wg handshake)
	PublicInterface  string   // empty: discover from the default route
	RevertSeconds    int      // auto-revert window unless confirmed
	PublicAllowPorts []string // extra public TCP ports (default none)
}

// ApplyResult echoes what was applied for the controller's record.
type ApplyResult struct {
	PublicInterface  string
	WGPort           int
	RevertSeconds    int
	PublicAllowPorts []string
}

// Apply loads the locked ruleset and ARMS the auto-revert. The lockdown is live
// immediately but undoes itself after RevertSeconds unless Confirm cancels it —
// the lockout-safety guarantee.
func Apply(ctx context.Context, cmd commands, params ApplyParams) (ApplyResult, error) {
	publicInterface, err := resolveInterface(ctx, cmd, params.PublicInterface)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := cmd.InstallFile(
		ctx, loadableRuleset(publicInterface, params.WGPort, params.PublicAllowPorts), stagingPath, "0600",
	); err != nil {
		return ApplyResult{}, err
	}
	if _, err := cmd.Run(ctx, "sudo nft -f {}", stagingPath); err != nil {
		return ApplyResult{}, err
	}
	if err := armRevert(ctx, cmd, params.RevertSeconds); err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{
		PublicInterface:  publicInterface,
		WGPort:           params.WGPort,
		RevertSeconds:    params.RevertSeconds,
		PublicAllowPorts: params.PublicAllowPorts,
	}, nil
}

// armRevert schedules `nft delete table inet atlas_mgmt` to run in `seconds` via a
// transient systemd timer (--collect so the unit is GC'd after it fires). Any prior
// armed revert is cleared first, so a re-apply re-arms cleanly. THIS IS THE SAFETY
// MECHANISM: if Confirm never arrives, the timer fires and open access is restored.
func armRevert(ctx context.Context, cmd commands, seconds int) error {
	cancelRevert(ctx, cmd)
	_, err := cmd.Run(
		ctx, "sudo systemd-run --collect {} {} {} nft delete table inet {}",
		fmt.Sprintf("--on-active=%d", seconds),
		"--unit="+revertUnit,
		"--description=Atlas management-firewall auto-revert (lockout safety)",
		table,
	)
	return err
}

// cancelRevert stops and clears the armed revert timer and service (best-effort).
func cancelRevert(ctx context.Context, cmd commands) {
	for _, unit := range []string{revertUnit + ".timer", revertUnit + ".service"} {
		cmd.RunUnchecked(ctx, "sudo systemctl stop {}", unit)
		cmd.RunUnchecked(ctx, "sudo systemctl reset-failed {}", unit)
	}
}

// --- Confirm ----------------------------------------------------------------

// ConfirmParams must carry the SAME interface/port/allow-ports Apply used, so the
// persisted ruleset matches the live one. PublicInterface empty means discover.
type ConfirmParams struct {
	WGPort           int
	PublicInterface  string
	PublicAllowPorts []string
}

// ConfirmResult reports the confirmation.
type ConfirmResult struct {
	Confirmed       bool
	PublicInterface string
}

// Confirm cancels the armed auto-revert and makes the locked ruleset the boot
// default (write the persisted include + enable the fail-closed boot unit). The
// live table is already loaded by Apply; this just makes it survive a reboot.
func Confirm(ctx context.Context, cmd commands, params ConfirmParams) (ConfirmResult, error) {
	publicInterface, err := resolveInterface(ctx, cmd, params.PublicInterface)
	if err != nil {
		return ConfirmResult{}, err
	}
	cancelRevert(ctx, cmd)
	if err := cmd.InstallDirectory(ctx, "/etc/atlas", "0755"); err != nil {
		return ConfirmResult{}, err
	}
	if err := cmd.InstallFile(
		ctx, loadableRuleset(publicInterface, params.WGPort, params.PublicAllowPorts), persistPath, "0644",
	); err != nil {
		return ConfirmResult{}, err
	}
	// Best-effort like the Python's check=False: a host without the boot unit staged
	// still keeps the live lockdown; the enable only affects reboot survival.
	cmd.RunUnchecked(ctx, "sudo systemctl enable {}", persistUnit)
	return ConfirmResult{Confirmed: true, PublicInterface: publicInterface}, nil
}

// --- Revert -----------------------------------------------------------------

// RevertParams is empty — reverting restores open access unconditionally.
type RevertParams struct{}

// RevertResult reports the revert ran.
type RevertResult struct {
	Reverted bool
}

// Revert restores open public access: cancel the armed auto-revert, delete the live
// atlas_mgmt table, and remove the persisted ruleset + disable the boot unit so a
// reboot does not re-lock. Every step is best-effort and idempotent — a
// half-reverted host converges cleanly — so Revert never fails.
func Revert(ctx context.Context, cmd commands, _ RevertParams) (RevertResult, error) {
	cancelRevert(ctx, cmd)
	cmd.RunUnchecked(ctx, "sudo nft delete table inet {}", table)
	cmd.RunUnchecked(ctx, "sudo rm -f {}", persistPath)
	cmd.RunUnchecked(ctx, "sudo systemctl disable {}", persistUnit)
	return RevertResult{Reverted: true}, nil
}

// --- interface discovery ----------------------------------------------------

// resolveInterface returns the requested public interface, or discovers it from the
// default route when none was given.
func resolveInterface(ctx context.Context, cmd commands, requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	return discoverPublicInterface(ctx, cmd)
}

// discoverPublicInterface is the default-route (uplink) device — the public
// interface to lock down. Ports mgmt_firewall.discover_public_interface /
// network_env.default_route_device (a read-only query, no sudo). Unlike the Python,
// which returns "" when there is no default route (and would then render an empty
// iifname), this fails loud: locking down an unnamed interface is not a lockdown,
// and the vmnetwork firewall makes the same choice.
func discoverPublicInterface(ctx context.Context, cmd commands) (string, error) {
	output, err := cmd.Run(ctx, "ip -j route show default")
	if err != nil {
		return "", err
	}
	var routes []struct {
		Device string `json:"dev"`
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil {
		return "", fmt.Errorf("could not parse `ip -j route show default`: %w", err)
	}
	if len(routes) == 0 || routes[0].Device == "" {
		return "", errors.New("no default route to discover the public interface from")
	}
	return routes[0].Device, nil
}
