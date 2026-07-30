package vmnetwork

import (
	"context"
	"testing"
)

// The firewall command builders, held to firewall.py's rendered output.
func TestFirewallCommandsRenderLikeThePython(t *testing.T) {
	rule443 := firewallRule{protocol: "tcp", port: 443}
	rule53 := firewallRule{protocol: "udp", port: 53}
	for _, testCase := range []struct{ name, got, want string }{
		{"ensure chain", ensureFirewallChainCommand(),
			"add chain inet atlas public_filter '{ type filter hook forward priority filter - 5; policy accept; }'"},
		{"established", establishedRuleCommand("eth0", "2001:db8::2"),
			"add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 ct state established,related accept"},
		{"tcp/443", portRuleCommand("eth0", "2001:db8::2", rule443),
			"add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 tcp dport 443 accept"},
		{"udp/53", portRuleCommand("eth0", "2001:db8::2", rule53),
			"add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 udp dport 53 accept"},
		{"drop", dropRuleCommand("eth0", "2001:db8::2"),
			"add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 drop"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.got != testCase.want {
				t.Errorf("rendered\n  %q\nwant\n  %q", testCase.got, testCase.want)
			}
		})
	}
}

func TestParseFirewallRuleRefusesBadTokens(t *testing.T) {
	for _, bad := range []string{"443", "tcp/", "tcp/notaport", "sctp/443", "tcp/0", "tcp/70000", "tcp/443/x"} {
		if _, err := parseFirewallRule(bad); err == nil {
			t.Errorf("parseFirewallRule(%q) accepted it", bad)
		}
	}
	rule, err := parseFirewallRule("udp/53")
	if err != nil || rule.protocol != "udp" || rule.port != 53 {
		t.Errorf("parseFirewallRule(udp/53) = %+v, %v", rule, err)
	}
}

// A firewall.env with two allowed ports installs the full block in order:
// established accept, one accept per rule, then the closing drop, in the dedicated
// higher-priority chain.
func TestApplyPersistedFirewallInstallsTheBlock(t *testing.T) {
	environment := "VIRTUAL_MACHINE_IPV6=2001:db8::2\nRULES=tcp/443 udp/53\n"
	fake := newFakeCommands().
		exists("sudo test -f "+firewallPath).
		output("sudo cat "+firewallPath, environment).
		output("ip -j -6 route show default", `[{"dev":"eth0"}]`)

	if err := applyPersistedFirewall(context.Background(), fake, firewallPath); err != nil {
		t.Fatalf("applyPersistedFirewall: %v", err)
	}
	assertTrace(t, fake.trace, []string{
		"? sudo test -f " + firewallPath,
		"sudo cat " + firewallPath,
		"ip -j -6 route show default",
		"? sudo nft list chain inet atlas public_filter",
		"sudo nft add chain inet atlas public_filter '{ type filter hook forward priority filter - 5; policy accept; }'",
		"- sudo nft -a list chain inet atlas public_filter",
		"sudo nft add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 ct state established,related accept",
		"sudo nft add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 tcp dport 443 accept",
		"sudo nft add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 udp dport 53 accept",
		"sudo nft add rule inet atlas public_filter iifname eth0 ip6 daddr 2001:db8::2 drop",
	})
}

// No firewall.env is a no-op — the VM stays fully public.
func TestApplyPersistedFirewallIsANoOpWithoutASidecar(t *testing.T) {
	fake := newFakeCommands()
	if err := applyPersistedFirewall(context.Background(), fake, firewallPath); err != nil {
		t.Fatalf("applyPersistedFirewall: %v", err)
	}
	assertTrace(t, fake.trace, []string{"? sudo test -f " + firewallPath})
}

// remove reverts the VM to public by scraping its rules off the dedicated chain.
func TestRemoveFirewallClearsTheVMsRules(t *testing.T) {
	const listing = "iifname \"eth0\" ip6 daddr 2001:db8::2 ct state established,related accept # handle 4\n" +
		"iifname \"eth0\" ip6 daddr 2001:db8::2 tcp dport 443 accept # handle 5\n" +
		"iifname \"eth0\" ip6 daddr 2001:db8::2 drop # handle 6\n"
	fake := newFakeCommands().
		exists("sudo nft list chain inet atlas public_filter").
		output("sudo nft -a list chain inet atlas public_filter", listing)

	if err := removeFirewall(context.Background(), fake, "2001:db8::2"); err != nil {
		t.Fatalf("removeFirewall: %v", err)
	}
	assertTrace(t, fake.trace, []string{
		"? sudo nft list chain inet atlas public_filter",
		"- sudo nft -a list chain inet atlas public_filter",
		"- sudo nft delete rule inet atlas public_filter handle 4",
		"- sudo nft delete rule inet atlas public_filter handle 5",
		"- sudo nft delete rule inet atlas public_filter handle 6",
	})
}
