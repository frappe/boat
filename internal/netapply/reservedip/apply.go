// The host-touching half: discover how the vendor delivers this host's reserved
// IPv4, install the DNAT/SNAT/forward rules that 1:1-NAT it to a guest, and (for
// the DigitalOcean anchor model) policy-route that guest's egress out the anchor
// gateway. Every mutation is idempotent — a re-run on a cold boot, a reconcile or
// a double attach changes nothing — and every teardown is best-effort, so a detach
// after the VM is already gone is not an error.
//
// Ported from apply_reserved_ip_nat / apply_routed_reserved_ip_nat /
// remove_reserved_ip_nat / discover_reserved_ip_anchor in
// scripts/lib/atlas/reserved_ip_nat.py. The one seam is `commands`; outside tests
// its only implementation is *run.Runner.
package reservedip

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/run"
)

// metadataAnchorBase is DigitalOcean's link-local metadata endpoint for the
// droplet's anchor IPv4 — the on-droplet handle for any reserved IP bound to it.
// Stable, no DNS. A host with no metadata service (a Self-Managed box handed a
// routed flexible IP instead) simply gets no answer, which is how one code path
// serves both delivery models.
const metadataAnchorBase = "http://169.254.169.254/metadata/v1/interfaces/public/0/anchor_ipv4"

// egressTable and its rule leave the guest's egress out the anchor gateway. Named
// here so the mutation and the teardown spell the same numbers.

// commands is the host-touching surface: one implementation in production, a
// recorder in tests. It is deliberately smaller than park's — no Probe, because
// every question this package asks guards a mutation that fails loudly on its own
// (spec: run.OK is free exactly there).
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
}

var _ commands = (*run.Runner)(nil)

// Anchor is the droplet's anchor IPv4 (address plus gateway) — DigitalOcean's
// on-droplet handle for a reserved IP. Inbound packets are destined to the
// address; egress is SNAT'd to it and routed via the gateway so DO's edge maps
// anchor→reserved.
type Anchor struct {
	Address string
	Gateway string
}

// Delivery is which model this host uses, returned so the caller's log line names
// the right one. Anchored is the DigitalOcean anchor path; the zero value is the
// routed flexible-IP path.
type Delivery struct {
	Anchored bool
	Anchor   Anchor
}

// Attach installs the 1:1 NAT for reservedIPv4 to the guest reachable at guestIPv4
// out hostVeth, discovering the delivery model from the host. Idempotent.
//
// The addresses are canonicalised here, at the boundary that renders them into nft
// rules, for the same reason park canonicalises a /128: nft re-lexes `;` and `#`
// out of a value even after run.Quote made it one shell token, so a value that is
// not exactly an IPv4 must not reach the ruleset.
func Attach(
	ctx context.Context, runner *run.Runner, guestIPv4 string, hostVeth string, reservedIPv4 string,
) (Delivery, error) {
	return newApplier(runner).attach(ctx, guestIPv4, hostVeth, reservedIPv4)
}

// Detach removes the guest's 1:1 NAT and its egress policy route, best-effort. It
// is keyed on the guest's private v4 — the one match common to all three rules —
// so a detach needs neither the reserved IP nor a fresh metadata read, and tears
// down both delivery models (the routed one simply has no policy route to drop).
func Detach(ctx context.Context, runner *run.Runner, guestIPv4 string) error {
	return newApplier(runner).detach(ctx, guestIPv4)
}

type applier struct {
	commands commands
}

func newApplier(runner *run.Runner) *applier {
	return &applier{commands: runner}
}

func (applier *applier) attach(
	ctx context.Context, guestIPv4 string, hostVeth string, reservedIPv4 string,
) (Delivery, error) {
	guest, ok := canonicalIPv4(guestIPv4)
	if !ok {
		return Delivery{}, fmt.Errorf("reserved-ip: %q is not the guest's IPv4 and cannot render into an nft rule", guestIPv4)
	}
	reserved, ok := canonicalIPv4(reservedIPv4)
	if !ok {
		return Delivery{}, fmt.Errorf("reserved-ip: %q is not an IPv4 and cannot render into an nft rule", reservedIPv4)
	}
	if !validInterfaceName(hostVeth) {
		return Delivery{}, fmt.Errorf("reserved-ip: %q is not a host interface name and cannot render into an nft rule", hostVeth)
	}

	anchor, anchored, err := applier.discoverAnchor(ctx)
	if err != nil {
		return Delivery{}, err
	}
	if err := applier.ensurePreroutingChain(ctx); err != nil {
		return Delivery{}, err
	}
	if anchored {
		// DigitalOcean: DNAT the anchor (what inbound is destined to), SNAT the
		// guest out as the anchor, and policy-route it via the anchor gateway.
		if err := applier.ensureRules(ctx, anchor.Address, guest, hostVeth); err != nil {
			return Delivery{}, err
		}
		if err := applier.applyEgressRoute(ctx, anchor, guest); err != nil {
			return Delivery{}, err
		}
		return Delivery{Anchored: true, Anchor: anchor}, nil
	}
	// Routed flexible IP: the packet arrives destined to the reserved IP itself, so
	// DNAT and SNAT both key on it, and egress needs no policy route.
	if err := applier.ensureRules(ctx, reserved, guest, hostVeth); err != nil {
		return Delivery{}, err
	}
	return Delivery{}, nil
}

// ensureRules installs the DNAT, the SNAT and the forward accept if absent, each
// guarded by a substring match against the live chain. mappedIPv4 is the anchor
// address (DigitalOcean) or the reserved IP itself (routed) — the destination the
// vendor delivers to and the source the guest's egress is mapped to.
func (applier *applier) ensureRules(ctx context.Context, mappedIPv4 string, guestIPv4 string, hostVeth string) error {
	prerouting, err := applier.commands.Run(ctx, "sudo nft list chain inet atlas {}", prerouting)
	if err != nil {
		return err
	}
	if !hasDNAT(prerouting, mappedIPv4, guestIPv4) {
		if _, err := applier.commands.Run(ctx, "sudo nft "+dnatRuleCommand(mappedIPv4, guestIPv4)); err != nil {
			return err
		}
	}
	postrouting, err := applier.commands.Run(ctx, "sudo nft list chain inet atlas {}", postrouting)
	if err != nil {
		return err
	}
	if !hasSNAT(postrouting, mappedIPv4, guestIPv4) {
		if _, err := applier.commands.Run(ctx, "sudo nft "+snatRuleCommand(mappedIPv4, guestIPv4)); err != nil {
			return err
		}
	}
	forward, err := applier.commands.Run(ctx, "sudo nft list chain inet atlas {}", forward)
	if err != nil {
		return err
	}
	if !hasForward(forward, guestIPv4, hostVeth) {
		if _, err := applier.commands.Run(ctx, "sudo nft "+forwardRuleCommand(guestIPv4, hostVeth)); err != nil {
			return err
		}
	}
	return nil
}

// ensurePreroutingChain creates the dstnat prerouting chain on demand. The network
// scaffold makes only the forward and srcnat-postrouting chains; inbound DNAT is
// the first thing that needs a prerouting nat hook.
func (applier *applier) ensurePreroutingChain(ctx context.Context) error {
	if applier.commands.OK(ctx, "sudo nft list chain inet atlas {}", prerouting) {
		return nil
	}
	_, err := applier.commands.Run(ctx, "sudo nft "+preroutingChainCommand())
	return err
}

func (applier *applier) detach(ctx context.Context, guestIPv4 string) error {
	guest, ok := canonicalIPv4(guestIPv4)
	if !ok {
		return fmt.Errorf("reserved-ip: %q is not the guest's IPv4", guestIPv4)
	}
	for _, chain := range []string{prerouting, postrouting, forward} {
		listing, err := applier.commands.RunUnchecked(ctx, "sudo nft -a list chain inet atlas {}", chain)
		if err != nil {
			return err
		}
		for _, handle := range handlesFor(listing, guest) {
			if _, err := applier.commands.RunUnchecked(ctx, "sudo nft delete rule inet atlas {} handle {}", chain, handle); err != nil {
				return err
			}
		}
	}
	return applier.removeEgressRoute(ctx, guest)
}

// discoverAnchor reads the droplet's anchor IPv4 from DigitalOcean metadata, or
// reports no anchor when there is none (a routed flexible IP). A metadata service
// that answers with something that is not an IPv4 is a failure, not a silent
// fallback: rendering a garbage anchor into the ruleset is exactly what
// canonicalIPv4 exists to stop.
func (applier *applier) discoverAnchor(ctx context.Context) (Anchor, bool, error) {
	if !applier.commands.OK(ctx, "curl -s --max-time 3 -o /dev/null {}", metadataAnchorBase+"/address") {
		return Anchor{}, false, nil
	}
	address, err := applier.commands.Run(ctx, "curl -s --max-time 5 {}", metadataAnchorBase+"/address")
	if err != nil {
		return Anchor{}, false, err
	}
	gateway, err := applier.commands.Run(ctx, "curl -s --max-time 5 {}", metadataAnchorBase+"/gateway")
	if err != nil {
		return Anchor{}, false, err
	}
	address, gateway = strings.TrimSpace(address), strings.TrimSpace(gateway)
	if address == "" || gateway == "" {
		return Anchor{}, false, nil
	}
	canonicalAddress, addressOK := canonicalIPv4(address)
	canonicalGateway, gatewayOK := canonicalIPv4(gateway)
	if !addressOK || !gatewayOK {
		return Anchor{}, false, fmt.Errorf(
			"reserved-ip: droplet metadata returned an anchor that is not an IPv4 (address %q, gateway %q)", address, gateway,
		)
	}
	return Anchor{Address: canonicalAddress, Gateway: canonicalGateway}, true, nil
}

// applyEgressRoute policy-routes this guest's egress out the anchor gateway so DO
// maps it to the reserved IP, scoped to `from <guest-v4>` so the host's own
// default route and every other VM's NAT44 egress are untouched. Idempotent: the
// route is `replace`, the rule added only when absent (ip rule has no replace).
func (applier *applier) applyEgressRoute(ctx context.Context, anchor Anchor, guestIPv4 string) error {
	uplink := applier.uplink(ctx)
	if uplink == "" {
		return fmt.Errorf("reserved-ip: no IPv4 default route, so the anchor gateway %s is not reachable to route egress through", anchor.Gateway)
	}
	if _, err := applier.commands.Run(
		ctx, "sudo ip -4 route replace default via {} dev {} table {}", anchor.Gateway, uplink, egressTableID,
	); err != nil {
		return err
	}
	rules, err := applier.commands.Run(ctx, "sudo ip -4 rule show")
	if err != nil {
		return err
	}
	if strings.Contains(rules, "from "+guestIPv4+" ") || strings.Contains(rules, "from "+guestIPv4+"\t") {
		return nil
	}
	_, err = applier.commands.Run(ctx, "sudo ip -4 rule add from {} lookup {}", guestIPv4, egressTableID)
	return err
}

// removeEgressRoute drops the guest's policy rule, best-effort. The table's default
// route is left — it is shared scaffolding keyed only by the rule, harmless when
// nothing points at it, and re-replaced on the next attach.
func (applier *applier) removeEgressRoute(ctx context.Context, guestIPv4 string) error {
	_, err := applier.commands.RunUnchecked(ctx, "sudo ip -4 rule del from {} lookup {}", guestIPv4, egressTableID)
	return err
}

// uplink is the interface carrying the IPv4 default route — where the anchor
// gateway is reachable. Read-only, so no sudo, and tolerant of a host with no
// default route (returns ""), the same shape park.uplink has for the v6 plane.
func (applier *applier) uplink(ctx context.Context) string {
	output, err := applier.commands.RunUnchecked(ctx, "ip -j -4 route show default")
	if err != nil {
		return ""
	}
	var routes []struct {
		Device string `json:"dev"`
	}
	if json.Unmarshal([]byte(output), &routes) != nil || len(routes) == 0 {
		return ""
	}
	return routes[0].Device
}

// validInterfaceName accepts only what a Linux interface name can be and nft can
// read as one token: non-empty, at most IFNAMSIZ-1 bytes, and nothing but the
// characters a device name uses. It is the hostVeth twin of canonicalIPv4 — the
// veth reaches `oifname <name>`, and a name carrying a space, a `;` or a `#` would
// inject there.
func validInterfaceName(name string) bool {
	if name == "" || len(name) > 15 {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		letter := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
		digit := character >= '0' && character <= '9'
		punctuation := character == '-' || character == '_' || character == '.' || character == '@'
		if !letter && !digit && !punctuation {
			return false
		}
	}
	return true
}
