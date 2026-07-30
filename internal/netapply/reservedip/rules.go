// The pure half of reserved-IP 1:1 NAT: the nft rules that DNAT an inbound public
// IPv4 to a guest's private /30 and SNAT that guest's egress back out as the
// public address, plus the substring matches that make each install idempotent
// and each teardown handle-scraped. Nothing here touches a host, which is what
// lets the exact rendered rule be asserted on a laptop against the Python's.
//
// Ported from scripts/lib/atlas/reserved_ip_nat.py; the chapter is
// spec/06-networking.md, and spec/33-boat.md §6.1 puts this row in Boat.
package reservedip

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/frappe/boat/internal/run"
)

const (
	// The nft chains this module writes into. The table (inet atlas) and the
	// forward chain are the network bring-up's scaffold; the srcnat postrouting
	// chain is the NAT44 masquerade's. Only the dstnat prerouting chain is created
	// on demand here — inbound DNAT is the first thing on the host that needs a
	// prerouting nat hook.
	prerouting  = "prerouting"
	postrouting = "postrouting"
	forward     = "forward"

	// preroutingChainSpecification is the chain clause for the dstnat hook. It goes
	// through a run.Quote so its spaces and `;` reach nft as ONE argv token, the
	// same trap park's forward-chain clause threads.
	preroutingChainSpecification = "{ type nat hook prerouting priority dstnat; policy accept; }"

	// egressTableID is the policy-routing table a DigitalOcean reserved IP's guest
	// egress is sent out through, so it leaves via the anchor gateway and DO's edge
	// maps anchor→reserved. One reserved IP per host today, so one table suffices;
	// the `from <guest-v4>` rule scopes it to that guest. The routed (Self-Managed)
	// model needs no policy route at all — the reserved IP is genuinely routed to
	// the host, so egress leaves over the normal default route.
	egressTableID = "100"
)

// canonicalIPv4 parses address and returns its canonical text, or false when it
// is not an ordinary IPv4 address.
//
// This is reserved-IP NAT's first line of defence, and it is park.canonicalAddress
// for the v4 plane. The anchor and the reserved IP are spliced into `nft add rule`
// and `ip -4 route`, and nft re-lexes its whole argument vector — it reads `;` as
// a statement separator and `#` as a comment — so a value carrying either injects
// into the ruleset even after run.Quote has made it one shell token. netip.ParseAddr
// admits only what is actually an address and .String() re-emits it as nothing but
// digits and dots, so a malformed value cannot render. IPv4-in-IPv6 is refused:
// every address on this plane is a bare v4, and admitting a mapped form would widen
// what "an address" means for no caller that needs it.
func canonicalIPv4(address string) (string, bool) {
	parsed, err := netip.ParseAddr(address)
	if err != nil || !parsed.Is4() {
		return "", false
	}
	return parsed.String(), true
}

// preroutingChainCommand adds the dstnat prerouting chain, created on demand. The
// brace clause goes through run.Quote so it is one token to nft.
func preroutingChainCommand() string {
	return "add chain inet atlas " + prerouting + " " + run.Quote(preroutingChainSpecification)
}

// dnatRuleCommand rewrites the destination the vendor actually delivers a
// reserved-IP packet to — the anchor IP for DigitalOcean, the reserved IP itself
// for a routed flexible IP — to the guest's private /30 address, so routing then
// carries it across the veth into the namespace and out the tap. No input-interface
// match: the packet arrives on the uplink and we do not pin the iif.
func dnatRuleCommand(destinationIPv4 string, guestIPv4 string) string {
	return fmt.Sprintf(
		"add rule inet atlas %s ip daddr %s dnat to %s",
		prerouting, run.Quote(destinationIPv4), run.Quote(guestIPv4),
	)
}

// snatRuleCommand sources this guest's egress as the mapped public address, and is
// INSERTED at the chain head so it is evaluated before the host-wide 100.64.0.0/16
// masquerade. For the DigitalOcean model the mapped address is the anchor IP (DO's
// edge then maps anchor→reserved, so the world sees the reserved IP); for the routed
// model it is the reserved IP itself.
func snatRuleCommand(mappedIPv4 string, guestIPv4 string) string {
	return fmt.Sprintf(
		"insert rule inet atlas %s ip saddr %s snat to %s",
		postrouting, run.Quote(guestIPv4), run.Quote(mappedIPv4),
	)
}

// forwardRuleCommand accepts the inbound (post-DNAT) flow toward the guest's
// private v4 out the host-side veth. The forward chain is policy accept today, so
// this is belt-and-suspenders — but it keeps the inbound v4 path explicit and
// survives a future per-VM firewall that flips the policy to drop.
func forwardRuleCommand(guestIPv4 string, hostVeth string) string {
	return fmt.Sprintf(
		"add rule inet atlas %s ip daddr %s oifname %s accept",
		forward, run.Quote(guestIPv4), run.Quote(hostVeth),
	)
}

// hasDNAT / hasSNAT / hasForward answer "is this rule already installed" against a
// live chain listing. Idempotency is by substring match rather than handle
// tracking: the match keys (the mapped/destination IP and the guest /30 address)
// are unique per guest, so the lines are an exact enough fingerprint and it
// survives nft re-rendering the rule text — the same contract park.armTrap keys on
// its counter name.
func hasDNAT(listing string, destinationIPv4 string, guestIPv4 string) bool {
	return anyLine(listing, func(line string) bool {
		return strings.Contains(line, destinationIPv4) && strings.Contains(line, guestIPv4) && strings.Contains(line, "dnat")
	})
}

func hasSNAT(listing string, mappedIPv4 string, guestIPv4 string) bool {
	return anyLine(listing, func(line string) bool {
		return strings.Contains(line, mappedIPv4) && strings.Contains(line, guestIPv4) && strings.Contains(line, "snat")
	})
}

func hasForward(listing string, guestIPv4 string, hostVeth string) bool {
	return anyLine(listing, func(line string) bool {
		return strings.Contains(line, guestIPv4) && strings.Contains(line, hostVeth)
	})
}

// handlesFor is the trailing handle number of every rule mentioning this guest's
// v4 — the one match key common to the DNAT, the SNAT and the forward accept — so
// a detach tears all three down without needing the anchor. `nft -a` prints
// `... # handle N`; the handle is the last token. Every match is returned, so a
// chain that somehow holds two copies is left with none. Mirrors park.ruleHandles.
func handlesFor(listing string, guestIPv4 string) []string {
	var handles []string
	for line := range strings.Lines(listing) {
		if !strings.Contains(line, guestIPv4) || !strings.Contains(line, "handle") {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			handles = append(handles, fields[len(fields)-1])
		}
	}
	return handles
}

func anyLine(listing string, predicate func(string) bool) bool {
	for line := range strings.Lines(listing) {
		if predicate(line) {
			return true
		}
	}
	return false
}
