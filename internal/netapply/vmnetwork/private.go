package vmnetwork

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/frappe/boat/internal/run"
)

// The private plane's tenant-isolation rules (design §4, spec/25), added to the
// host root-ns `inet atlas` forward chain as a pure function of the VM's own row.
// Each VM has its own veth, so a packet's source is physically attributable at the
// veth and nftables suffices — no eBPF. The plane is fail-closed: one terminal
// `fdaa::/16 drop` makes it default-deny, and the four per-VM rules are allow-by-
// exception, inserted ABOVE that drop.
//
// Ported from scripts/lib/atlas/private_network.py. Security-critical: a rule that
// renders wrong is a cross-tenant leak, so the four rules and the terminal drop are
// held to the Python's nft text byte for byte, golden-tested and differentiated on
// a host.
const (
	privatePlane = "fdaa::/16"
	// meshDevice is the WireGuard host mesh; a decap'd packet arrives on it.
	meshDevice = "wg-mesh"
	// The infra /48 (proxy/resolver). The command sends fdaa:0:0::/48; nft lists it
	// back canonicalised as fdaa::/48, which is what the idempotency guard matches.
	infraPrefixInCommand = "fdaa:0:0::/48"
	infraPrefixCanonical = "fdaa::/48"
)

// applyPrivateNetwork installs this VM's four isolation rules, re-asserting the
// terminal drop first so the plane is default-deny even on a host whose scaffold
// predates the feature. Idempotent: each rule is inserted only when its canonical
// nft text is absent from the live chain. The guards match the QUOTED interface
// name nft prints, so unlike the public forward rules this needs no un-quoting —
// the Python already keys on the quoted form here.
func applyPrivateNetwork(ctx context.Context, commands commands, veth, privateAddress, tenantPrefix string) error {
	listing, err := commands.RunUnchecked(ctx, "sudo nft list chain inet atlas forward")
	if err != nil {
		return err
	}
	if !strings.Contains(listing, terminalDropText()) {
		if _, err := commands.Run(ctx, "sudo nft "+terminalDropCommand()); err != nil {
			return err
		}
	}
	// Re-read: the terminal add above changed the chain, and the per-VM guards
	// compare against the same canonical text.
	listing, err = commands.RunUnchecked(ctx, "sudo nft list chain inet atlas forward")
	if err != nil {
		return err
	}
	rules := []struct{ text, command string }{
		{antiSpoofText(veth, privateAddress), antiSpoofCommand(veth, privateAddress)},
		{sameTenantEgressText(veth, privateAddress, tenantPrefix), sameTenantEgressCommand(veth, privateAddress, tenantPrefix)},
		{infraDestinationText(veth, privateAddress), infraDestinationCommand(veth, privateAddress)},
		{crossHostDeliveryText(veth, privateAddress, tenantPrefix), crossHostDeliveryCommand(veth, privateAddress, tenantPrefix)},
	}
	for _, rule := range rules {
		if strings.Contains(listing, rule.text) {
			continue
		}
		if _, err := commands.Run(ctx, "sudo nft "+rule.command); err != nil {
			return err
		}
	}
	return nil
}

// removePrivateNetwork deletes this VM's per-VM rules by handle — every forward
// rule mentioning either the VM's private /128 or its veth (the terminal drop
// mentions neither, so it stays). Best-effort, keyed on either match.
func removePrivateNetwork(ctx context.Context, commands commands, privateAddress, veth string) error {
	listing, err := commands.RunUnchecked(ctx, "sudo nft -a list chain inet atlas forward")
	if err != nil {
		return err
	}
	for _, handle := range privateHandles(listing, privateAddress, veth) {
		if _, err := commands.RunUnchecked(ctx, "sudo nft delete rule inet atlas forward handle {}", handle); err != nil {
			return err
		}
	}
	return nil
}

// privateHandles is the trailing handle of every forward rule mentioning the VM's
// /128 or its quoted veth. `nft -a` prints `... # handle N`.
func privateHandles(listing, privateAddress, veth string) []string {
	quotedVeth := "\"" + veth + "\""
	var handles []string
	for line := range strings.Lines(listing) {
		if !strings.Contains(line, "handle") {
			continue
		}
		if !strings.Contains(line, privateAddress) && !strings.Contains(line, quotedVeth) {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			handles = append(handles, fields[len(fields)-1])
		}
	}
	return handles
}

// --- rule text as `nft list` prints it (the idempotency guards) — veth quoted ---

func terminalDropText() string {
	return "ip6 daddr " + privatePlane + " drop"
}

func antiSpoofText(veth, privateAddress string) string {
	return fmt.Sprintf(`iifname "%s" ip6 daddr %s ip6 saddr != %s drop`, veth, privatePlane, privateAddress)
}

func sameTenantEgressText(veth, privateAddress, tenantPrefix string) string {
	return fmt.Sprintf(`iifname "%s" ip6 saddr %s ip6 daddr %s accept`, veth, privateAddress, tenantPrefix)
}

func infraDestinationText(veth, privateAddress string) string {
	return fmt.Sprintf(`iifname "%s" ip6 saddr %s ip6 daddr %s accept`, veth, privateAddress, infraPrefixCanonical)
}

func crossHostDeliveryText(veth, privateAddress, tenantPrefix string) string {
	return fmt.Sprintf(`iifname "%s" oifname "%s" ip6 saddr %s ip6 daddr %s accept`, meshDevice, veth, tenantPrefix, privateAddress)
}

// --- nft command builders (values run.Quote'd, keywords bare) ---

func terminalDropCommand() string {
	return "add rule inet atlas forward ip6 daddr " + privatePlane + " drop"
}

func antiSpoofCommand(veth, privateAddress string) string {
	return fmt.Sprintf("insert rule inet atlas forward iifname %s ip6 daddr %s ip6 saddr != %s drop",
		run.Quote(veth), privatePlane, run.Quote(privateAddress))
}

func sameTenantEgressCommand(veth, privateAddress, tenantPrefix string) string {
	return fmt.Sprintf("insert rule inet atlas forward iifname %s ip6 saddr %s ip6 daddr %s accept",
		run.Quote(veth), run.Quote(privateAddress), run.Quote(tenantPrefix))
}

func infraDestinationCommand(veth, privateAddress string) string {
	return fmt.Sprintf("insert rule inet atlas forward iifname %s ip6 saddr %s ip6 daddr %s accept",
		run.Quote(veth), run.Quote(privateAddress), infraPrefixInCommand)
}

func crossHostDeliveryCommand(veth, privateAddress, tenantPrefix string) string {
	return fmt.Sprintf("insert rule inet atlas forward iifname %s oifname %s ip6 saddr %s ip6 daddr %s accept",
		meshDevice, run.Quote(veth), run.Quote(tenantPrefix), run.Quote(privateAddress))
}

// canonicalIPv6Prefix admits an IPv6 prefix (the tenant /48) and re-emits it in
// nft's canonical form, so the guard text matches what nft lists. Refuses anything
// that is not one before it reaches a rule.
func canonicalIPv6Prefix(prefix string) (string, bool) {
	parsed, err := netip.ParsePrefix(prefix)
	if err != nil || !parsed.Addr().Is6() || parsed.Addr().Is4In6() {
		return "", false
	}
	return parsed.String(), true
}
