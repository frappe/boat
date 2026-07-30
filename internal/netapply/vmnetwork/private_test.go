package vmnetwork

import (
	"context"
	"testing"
)

const (
	testVeth         = "atlas-hdeadbe"
	testPrivate      = "fdaa:1a2b:3c4d:0:1:2:3:4"
	testTenantPrefix = "fdaa:1a2b:3c4d::/48"
)

// The commands and the list-form texts captured from private_network.py. A
// rendering difference here is a cross-tenant leak, so both are held byte for byte:
// the command is what is sent, the text is what nft prints back and the idempotency
// guard matches.
func TestPrivatePlaneRendersLikeThePython(t *testing.T) {
	for _, testCase := range []struct{ name, got, want string }{
		{"terminal drop command", terminalDropCommand(), "add rule inet atlas forward ip6 daddr fdaa::/16 drop"},
		{"anti-spoof command", antiSpoofCommand(testVeth, testPrivate),
			"insert rule inet atlas forward iifname atlas-hdeadbe ip6 daddr fdaa::/16 ip6 saddr != fdaa:1a2b:3c4d:0:1:2:3:4 drop"},
		{"same-tenant command", sameTenantEgressCommand(testVeth, testPrivate, testTenantPrefix),
			"insert rule inet atlas forward iifname atlas-hdeadbe ip6 saddr fdaa:1a2b:3c4d:0:1:2:3:4 ip6 daddr fdaa:1a2b:3c4d::/48 accept"},
		{"infra command sends the un-canonical /48", infraDestinationCommand(testVeth, testPrivate),
			"insert rule inet atlas forward iifname atlas-hdeadbe ip6 saddr fdaa:1a2b:3c4d:0:1:2:3:4 ip6 daddr fdaa:0:0::/48 accept"},
		{"cross-host command", crossHostDeliveryCommand(testVeth, testPrivate, testTenantPrefix),
			"insert rule inet atlas forward iifname wg-mesh oifname atlas-hdeadbe ip6 saddr fdaa:1a2b:3c4d::/48 ip6 daddr fdaa:1a2b:3c4d:0:1:2:3:4 accept"},

		{"terminal drop text", terminalDropText(), "ip6 daddr fdaa::/16 drop"},
		{"anti-spoof text", antiSpoofText(testVeth, testPrivate),
			`iifname "atlas-hdeadbe" ip6 daddr fdaa::/16 ip6 saddr != fdaa:1a2b:3c4d:0:1:2:3:4 drop`},
		{"same-tenant text", sameTenantEgressText(testVeth, testPrivate, testTenantPrefix),
			`iifname "atlas-hdeadbe" ip6 saddr fdaa:1a2b:3c4d:0:1:2:3:4 ip6 daddr fdaa:1a2b:3c4d::/48 accept`},
		{"infra text matches the canonical /48 nft prints", infraDestinationText(testVeth, testPrivate),
			`iifname "atlas-hdeadbe" ip6 saddr fdaa:1a2b:3c4d:0:1:2:3:4 ip6 daddr fdaa::/48 accept`},
		{"cross-host text", crossHostDeliveryText(testVeth, testPrivate, testTenantPrefix),
			`iifname "wg-mesh" oifname "atlas-hdeadbe" ip6 saddr fdaa:1a2b:3c4d::/48 ip6 daddr fdaa:1a2b:3c4d:0:1:2:3:4 accept`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.got != testCase.want {
				t.Errorf("rendered\n  %q\nwant\n  %q", testCase.got, testCase.want)
			}
		})
	}
}

// A fresh chain: the terminal drop and all four per-VM rules install, the four via
// insert (head) above the drop.
func TestApplyPrivateNetworkInstallsTheTerminalDropAndFourRules(t *testing.T) {
	fake := newFakeCommands()
	if err := applyPrivateNetwork(context.Background(), fake, testVeth, testPrivate, testTenantPrefix); err != nil {
		t.Fatalf("applyPrivateNetwork: %v", err)
	}
	assertTrace(t, fake.trace, []string{
		"- sudo nft list chain inet atlas forward",
		"sudo nft add rule inet atlas forward ip6 daddr fdaa::/16 drop",
		"- sudo nft list chain inet atlas forward",
		"sudo nft insert rule inet atlas forward iifname atlas-hdeadbe ip6 daddr fdaa::/16 ip6 saddr != fdaa:1a2b:3c4d:0:1:2:3:4 drop",
		"sudo nft insert rule inet atlas forward iifname atlas-hdeadbe ip6 saddr fdaa:1a2b:3c4d:0:1:2:3:4 ip6 daddr fdaa:1a2b:3c4d::/48 accept",
		"sudo nft insert rule inet atlas forward iifname atlas-hdeadbe ip6 saddr fdaa:1a2b:3c4d:0:1:2:3:4 ip6 daddr fdaa:0:0::/48 accept",
		"sudo nft insert rule inet atlas forward iifname wg-mesh oifname atlas-hdeadbe ip6 saddr fdaa:1a2b:3c4d::/48 ip6 daddr fdaa:1a2b:3c4d:0:1:2:3:4 accept",
	})
}

// A re-run on a chain that already holds them adds nothing: the guards match the
// quoted nft list text, so a restart does not duplicate a tenant-isolation rule.
func TestApplyPrivateNetworkIsIdempotent(t *testing.T) {
	installed := terminalDropText() + "\n" +
		antiSpoofText(testVeth, testPrivate) + "\n" +
		sameTenantEgressText(testVeth, testPrivate, testTenantPrefix) + "\n" +
		infraDestinationText(testVeth, testPrivate) + "\n" +
		crossHostDeliveryText(testVeth, testPrivate, testTenantPrefix) + "\n"
	fake := newFakeCommands().output("sudo nft list chain inet atlas forward", installed)

	if err := applyPrivateNetwork(context.Background(), fake, testVeth, testPrivate, testTenantPrefix); err != nil {
		t.Fatalf("applyPrivateNetwork: %v", err)
	}
	for _, command := range fake.trace {
		if len(command) > 2 && command[:2] != "- " {
			t.Errorf("a re-run mutated the chain: %q", command)
		}
	}
}

// Teardown scrapes every rule mentioning the /128 or the quoted veth, and leaves
// the host-wide terminal drop (which mentions neither).
func TestRemovePrivateNetworkDeletesByHandleLeavingTheTerminalDrop(t *testing.T) {
	const listing = "ip6 daddr fdaa::/16 drop # handle 3\n" +
		`iifname "atlas-hdeadbe" ip6 daddr fdaa::/16 ip6 saddr != fdaa:1a2b:3c4d:0:1:2:3:4 drop # handle 12` + "\n" +
		`iifname "wg-mesh" oifname "atlas-hdeadbe" ip6 saddr fdaa:1a2b:3c4d::/48 ip6 daddr fdaa:1a2b:3c4d:0:1:2:3:4 accept # handle 15` + "\n"
	fake := newFakeCommands().output("sudo nft -a list chain inet atlas forward", listing)

	if err := removePrivateNetwork(context.Background(), fake, testPrivate, testVeth); err != nil {
		t.Fatalf("removePrivateNetwork: %v", err)
	}
	assertTrace(t, fake.trace, []string{
		"- sudo nft -a list chain inet atlas forward",
		"- sudo nft delete rule inet atlas forward handle 12",
		"- sudo nft delete rule inet atlas forward handle 15",
	})
}

func TestCanonicalIPv6PrefixRefusesNonPrefixes(t *testing.T) {
	for _, bad := range []string{"fdaa:1a2b:3c4d::/48; drop", "not-a-prefix", "10.0.0.0/8", "fdaa:1a2b:3c4d::4"} {
		if _, ok := canonicalIPv6Prefix(bad); ok {
			t.Errorf("canonicalIPv6Prefix accepted %q", bad)
		}
	}
	if got, ok := canonicalIPv6Prefix("fdaa:1a2b:3c4d::/48"); !ok || got != "fdaa:1a2b:3c4d::/48" {
		t.Errorf("canonicalIPv6Prefix(valid) = %q, %v", got, ok)
	}
}
