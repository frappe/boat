package vmnetwork

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/sidecar"
)

// A VM's public-ingress firewall (spec/20). By default a VM's public /128 is
// reachable on every port; a firewall restricts that to a chosen set, in a
// SEPARATE higher-priority `public_filter` chain scoped to the uplink — so a
// tunnel (which arrives on wg-…, not the uplink) keeps full access for free, and
// the VM's own return traffic is accepted by conntrack. No firewall.env is a
// no-op: the VM stays fully public.
//
// Ported from scripts/lib/atlas/firewall.py. Security-relevant: a firewall that
// silently fails to re-apply on a restart re-opens the VM, so the rules are held
// to the Python's nft byte for byte.
const (
	publicFilterChain    = "public_filter"
	publicFilterPriority = "filter - 5"
)

// firewallRule is one allowed public ingress: a transport protocol and a port.
type firewallRule struct {
	protocol string
	port     int
}

func parseFirewallRule(token string) (firewallRule, error) {
	protocol, port, found := strings.Cut(token, "/")
	if !found || (protocol != "tcp" && protocol != "udp") {
		return firewallRule{}, fmt.Errorf("firewall rule %q: expected tcp/PORT or udp/PORT", token)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return firewallRule{}, fmt.Errorf("firewall rule %q: port must be an integer in 1-65535", token)
	}
	return firewallRule{protocol: protocol, port: number}, nil
}

// firewallConfig is one VM's durable firewall — its /128 and the allowed rules.
// An empty rule set is meaningful: deny all public ingress (reachable only over a
// tunnel).
type firewallConfig struct {
	virtualMachine string
	rules          []firewallRule
}

func parseFirewallConfig(text string) (firewallConfig, error) {
	virtualMachine, ok := canonicalIPv6(sidecar.Value(text, virtualMachineKey))
	if !ok {
		return firewallConfig{}, fmt.Errorf("firewall.env: %s is not a canonical IPv6 address", virtualMachineKey)
	}
	config := firewallConfig{virtualMachine: virtualMachine}
	for _, token := range strings.Fields(sidecar.Value(text, "RULES")) {
		rule, err := parseFirewallRule(token)
		if err != nil {
			return firewallConfig{}, err
		}
		config.rules = append(config.rules, rule)
	}
	return config, nil
}

// applyPersistedFirewall re-applies a VM's firewall from its sidecar at cold boot,
// after the VM's /128 host route exists. Absent sidecar (no firewall) is a no-op.
func applyPersistedFirewall(ctx context.Context, commands commands, firewallEnvironmentPath string) error {
	if !commands.OK(ctx, "sudo test -f {}", firewallEnvironmentPath) {
		return nil
	}
	text, err := commands.Run(ctx, "sudo cat {}", firewallEnvironmentPath)
	if err != nil {
		return err
	}
	config, err := parseFirewallConfig(text)
	if err != nil {
		return err
	}
	return applyFirewall(ctx, commands, config)
}

// applyFirewall installs the VM's block idempotently: ensure the chain, clear this
// VM's existing rules, then append established-accept, one accept per allowed rule,
// and a closing drop. Re-running converges to the same block. The uplink is
// discovered fresh, matching the Python.
func applyFirewall(ctx context.Context, commands commands, config firewallConfig) error {
	uplink, err := firewallUplink(ctx, commands)
	if err != nil {
		return err
	}
	if !commands.OK(ctx, "sudo nft list chain inet atlas {}", publicFilterChain) {
		if _, err := commands.Run(ctx, "sudo nft "+ensureFirewallChainCommand()); err != nil {
			return err
		}
	}
	if err := clearFirewallRules(ctx, commands, config.virtualMachine); err != nil {
		return err
	}
	if _, err := commands.Run(ctx, "sudo nft "+establishedRuleCommand(uplink, config.virtualMachine)); err != nil {
		return err
	}
	for _, rule := range config.rules {
		if _, err := commands.Run(ctx, "sudo nft "+portRuleCommand(uplink, config.virtualMachine, rule)); err != nil {
			return err
		}
	}
	_, err = commands.Run(ctx, "sudo nft "+dropRuleCommand(uplink, config.virtualMachine))
	return err
}

// removeFirewall reverts the VM to fully public, best-effort: a missing chain or
// absent block is not an error, symmetric with the down path.
func removeFirewall(ctx context.Context, commands commands, virtualMachine string) error {
	if !commands.OK(ctx, "sudo nft list chain inet atlas {}", publicFilterChain) {
		return nil
	}
	return clearFirewallRules(ctx, commands, virtualMachine)
}

// clearFirewallRules deletes every public_filter rule for this VM by handle. The
// chain is dedicated to public filtering and every rule is daddr-scoped to one VM,
// so the /128 is an exact discriminator.
func clearFirewallRules(ctx context.Context, commands commands, virtualMachine string) error {
	listing, err := commands.RunUnchecked(ctx, "sudo nft -a list chain inet atlas {}", publicFilterChain)
	if err != nil {
		return err
	}
	for _, handle := range handlesFor(listing, virtualMachine) {
		if _, err := commands.RunUnchecked(ctx, "sudo nft delete rule inet atlas {} handle {}", publicFilterChain, handle); err != nil {
			return err
		}
	}
	return nil
}

// firewallUplink is the v6 default-route device the firewall rules scope to. Run,
// not OK: the device IS the answer, and an absent one must fail loud rather than
// render an empty iifname.
func firewallUplink(ctx context.Context, commands commands) (string, error) {
	output, err := commands.Run(ctx, "ip -j -6 route show default")
	if err != nil {
		return "", err
	}
	device, err := firstRouteDevice(output)
	if err != nil {
		return "", fmt.Errorf("no IPv6 default route to scope the firewall to: %w", err)
	}
	return device, nil
}

// --- command builders (values run.Quote'd; the chain clause one token) ---

func ensureFirewallChainCommand() string {
	clause := "{ type filter hook forward priority " + publicFilterPriority + "; policy accept; }"
	return "add chain inet atlas " + publicFilterChain + " " + run.Quote(clause)
}

func establishedRuleCommand(uplink, virtualMachine string) string {
	return fmt.Sprintf("add rule inet atlas %s iifname %s ip6 daddr %s ct state established,related accept",
		publicFilterChain, run.Quote(uplink), run.Quote(virtualMachine))
}

func portRuleCommand(uplink, virtualMachine string, rule firewallRule) string {
	return fmt.Sprintf("add rule inet atlas %s iifname %s ip6 daddr %s %s dport %s accept",
		publicFilterChain, run.Quote(uplink), run.Quote(virtualMachine), rule.protocol, strconv.Itoa(rule.port))
}

func dropRuleCommand(uplink, virtualMachine string) string {
	return fmt.Sprintf("add rule inet atlas %s iifname %s ip6 daddr %s drop",
		publicFilterChain, run.Quote(uplink), run.Quote(virtualMachine))
}
