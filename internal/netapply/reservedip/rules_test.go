package reservedip

import "testing"

// The wanted strings are what reserved_ip_nat.py's command builders actually
// rendered for these inputs (captured via shlex-backed _substitute). A rendering
// difference here is not a wrong string — it is a reserved IP that lands nowhere
// or a guest whose egress is not seen as the reserved address, so the rule is held
// to the Python's byte for byte.
func TestRuleCommandsMatchThePython(t *testing.T) {
	const (
		anchor   = "10.47.0.10"
		reserved = "146.190.11.153"
		guest    = "100.64.0.2"
		veth     = "veth-abc"
	)
	for _, testCase := range []struct{ name, got, want string }{
		{
			"prerouting chain", preroutingChainCommand(),
			"add chain inet atlas prerouting '{ type nat hook prerouting priority dstnat; policy accept; }'",
		},
		{
			"dnat (DigitalOcean anchor)", dnatRuleCommand(anchor, guest),
			"add rule inet atlas prerouting ip daddr 10.47.0.10 dnat to 100.64.0.2",
		},
		{
			"snat (DigitalOcean anchor)", snatRuleCommand(anchor, guest),
			"insert rule inet atlas postrouting ip saddr 100.64.0.2 snat to 10.47.0.10",
		},
		{
			"forward accept", forwardRuleCommand(guest, veth),
			"add rule inet atlas forward ip daddr 100.64.0.2 oifname veth-abc accept",
		},
		{
			"dnat (routed flexible IP)", dnatRuleCommand(reserved, guest),
			"add rule inet atlas prerouting ip daddr 146.190.11.153 dnat to 100.64.0.2",
		},
		{
			"snat (routed flexible IP)", snatRuleCommand(reserved, guest),
			"insert rule inet atlas postrouting ip saddr 100.64.0.2 snat to 146.190.11.153",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.got != testCase.want {
				t.Errorf("rendered %q, want %q", testCase.got, testCase.want)
			}
		})
	}
}

// A value that is not a bare IPv4 cannot become an nft rule. This is the v4 twin
// of the wake trap's address guard: nft re-lexes `;` and `#`, so the address is
// admitted only if it is nothing but an address.
func TestCanonicalIPv4RefusesEverythingThatIsNotOne(t *testing.T) {
	for _, address := range []string{
		"10.47.0.10; drop",  // an nft statement smuggled in
		"10.47.0.10 #",      // an nft comment
		"2001:db8::9",       // v6 on the v4 plane
		"::ffff:10.47.0.10", // v4-in-v6 mapped form
		"10.47.0.10/32",     // a prefix, not an address
		"not-an-ip",
		"",
	} {
		if canonical, ok := canonicalIPv4(address); ok {
			t.Errorf("canonicalIPv4(%q) accepted it as %q", address, canonical)
		}
	}
}

func TestCanonicalIPv4CanonicalisesARealAddress(t *testing.T) {
	if got, ok := canonicalIPv4("010.047.000.010"); ok || got != "" {
		// Leading zeros are not a canonical dotted-decimal octet; ParseAddr refuses
		// them, which is the behaviour we want (an ambiguous octet is not admitted).
		t.Errorf("canonicalIPv4 accepted a zero-padded octet: %q", got)
	}
	if got, ok := canonicalIPv4("146.190.11.153"); !ok || got != "146.190.11.153" {
		t.Errorf("canonicalIPv4(%q) = %q, %v; want it unchanged and accepted", "146.190.11.153", got, ok)
	}
}

// Idempotency and teardown both key on the guest's private /30 being unique per
// guest — the property that lets a substring fingerprint stand in for handle
// tracking on install, and a handle scrape find every rule to delete on detach.
func TestMatchAndHandleScrapeKeyOnTheUniqueGuestAddress(t *testing.T) {
	const listing = "" +
		"table inet atlas {\n" +
		"  chain prerouting {\n" +
		"    ip daddr 10.47.0.10 dnat to 100.64.0.2 # handle 7\n" +
		"  }\n" +
		"  chain postrouting {\n" +
		"    ip saddr 100.64.0.2 snat to 10.47.0.10 # handle 9\n" +
		"    ip saddr 100.64.0.0/16 masquerade # handle 3\n" +
		"  }\n" +
		"  chain forward {\n" +
		"    ip daddr 100.64.0.2 oifname veth-abc accept # handle 11\n" +
		"  }\n" +
		"}\n"

	if !hasDNAT(listing, "10.47.0.10", "100.64.0.2") {
		t.Error("hasDNAT missed an installed DNAT")
	}
	if !hasSNAT(listing, "10.47.0.10", "100.64.0.2") {
		t.Error("hasSNAT missed an installed SNAT")
	}
	if !hasForward(listing, "100.64.0.2", "veth-abc") {
		t.Error("hasForward missed an installed forward accept")
	}
	// A different guest's rules are not this guest's.
	if hasDNAT(listing, "10.47.0.10", "100.64.9.9") {
		t.Error("hasDNAT matched a different guest")
	}

	handles := handlesFor(listing, "100.64.0.2")
	want := []string{"7", "9", "11"}
	if len(handles) != len(want) {
		t.Fatalf("handlesFor = %v, want %v", handles, want)
	}
	for index := range want {
		if handles[index] != want[index] {
			t.Fatalf("handlesFor = %v, want %v", handles, want)
		}
	}
	// The host-wide masquerade (a different saddr) is left alone.
	if got := handlesFor(listing, "100.64.0.0/16"); len(got) != 1 || got[0] != "3" {
		t.Errorf("handlesFor(masquerade) = %v, want [3] only", got)
	}
}
