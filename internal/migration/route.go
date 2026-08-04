package migration

import (
	"context"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/netapply/localownership"
)

// The keep-address cutover route installs (spec/24 §2.2, §2.9): once the tunnel is
// up, the source repoints the /128's delivery onto it and the target policy-routes
// the guest's replies back up it. Plus the source-side private-plane withdrawal that
// must precede the target booting the same private /128.

// SourceForwardParams is the /128 whose delivery repoints onto the tunnel.
type SourceForwardParams struct {
	VirtualMachineIPv6 string
}

// SourceForwardResult reports the source is now forwarding.
type SourceForwardResult struct {
	Forwarding bool
}

// SourceForward is the source side of a keep-address cutover: point the VM's /128
// delivery at the forward tunnel instead of its now-torn-down veth. By the time this
// runs the source VM's unit is down and its teardown already deleted the veth route,
// proxy-NDP entry and rules — but the source still holds the /64, so inbound for the
// /128 still lands here. This re-establishes reachability onto the tunnel: an atomic
// route replace (no black hole), the two forward-chain rules, and the proxy-NDP
// re-assert. This is the point the forward becomes live and PERMANENT. Idempotent:
// route replace and duplicate-guarded nft adds re-assert cleanly. Ports
// scripts/migration-source-forward.py.
//
// The proxy-NDP re-assert is UNCONDITIONAL: the upstream switch delivers a /128 to a
// host ONLY while that host answers Neighbor Solicitations for it (proven on Scaleway
// — ingress was 0% until this was re-asserted), on EVERY provider. The Python's "0"
// escape hatch is dropped — gotcha #13 — so a missing uplink is a hard error rather
// than a silent skip that black-holes ingress.
func SourceForward(ctx context.Context, cmd commands, uuid string, params SourceForwardParams) (SourceForwardResult, error) {
	device, err := TunnelDevice(uuid)
	if err != nil {
		return SourceForwardResult{}, err
	}
	if !cmd.OK(ctx, "ip link show {}", device) {
		return SourceForwardResult{}, errTunnelDown(device)
	}
	vmv6 := params.VirtualMachineIPv6

	// 1. Route the /128 into the tunnel — atomic replace, no delete-then-add gap.
	if _, err := cmd.Run(ctx, "sudo ip -6 route replace {} dev {}", vmv6+"/128", device); err != nil {
		return SourceForwardResult{}, err
	}
	// 2. Forward chain: admit inbound toward the tunnel and the reply coming back.
	if err := ensureForwardRule(ctx, cmd, "daddr", vmv6, "oifname", device); err != nil {
		return SourceForwardResult{}, err
	}
	if err := ensureForwardRule(ctx, cmd, "saddr", vmv6, "iifname", device); err != nil {
		return SourceForwardResult{}, err
	}
	// 3. Re-answer NDP for the /128 on the uplink.
	uplink, err := uplinkRequired(ctx, cmd)
	if err != nil {
		return SourceForwardResult{}, err
	}
	if _, err := cmd.Run(ctx, "sudo ip -6 neigh replace proxy {} dev {}", vmv6, uplink); err != nil {
		return SourceForwardResult{}, err
	}
	return SourceForwardResult{Forwarding: true}, nil
}

// ensureForwardRule adds a forward-chain rule unless an identical match already
// exists — nft has no native "add if absent", so the chain is listed and the rendered
// match substring-checked (the same guard vm-network-up makes). direction is
// daddr/saddr and ifKeyword oifname/iifname; the values are a validated /128 and a
// derived device, so they compose the rule directly.
func ensureForwardRule(ctx context.Context, cmd commands, direction, vmv6, ifKeyword, device string) error {
	match := fmt.Sprintf("ip6 %s %s %s %s accept", direction, vmv6, ifKeyword, device)
	chain, _ := cmd.RunUnchecked(ctx, "sudo nft list chain inet atlas forward")
	if strings.Contains(chain, match) {
		return nil
	}
	_, err := cmd.Run(ctx, "sudo nft add rule inet atlas forward ip6 {} {} {} {} accept", direction, vmv6, ifKeyword, device)
	return err
}

// TargetReceiveParams is the /128 whose egress is policy-routed up the tunnel.
type TargetReceiveParams struct {
	VirtualMachineIPv6 string
}

// TargetReceiveResult reports the return route is installed.
type TargetReceiveResult struct {
	Receiving bool
}

// TargetReceive is the target side of a keep-address cutover: the return-route policy
// that forces the guest's replies back UP the tunnel. Inbound already flows source →
// tunnel → target veth → guest once SourceForward is installed; the problem is
// EGRESS — the target sourcing the VM's /128 (which belongs to the SOURCE's /64) is
// dropped at the switch. So a per-VM policy route steers packets FROM the /128 to a
// private table whose only route sends them out the tunnel; everything else on the
// host is unaffected. This runs BEFORE SourceForward so the return path exists before
// inbound starts arriving. Idempotent: the rule add is duplicate-guarded (rules
// stack, unlike route replace), the table's default is a replace. The table id is
// DERIVED from the UUID. Ports scripts/migration-target-receive.py.
func TargetReceive(ctx context.Context, cmd commands, uuid string, params TargetReceiveParams) (TargetReceiveResult, error) {
	device, err := TunnelDevice(uuid)
	if err != nil {
		return TargetReceiveResult{}, err
	}
	table, err := TunnelTable(uuid)
	if err != nil {
		return TargetReceiveResult{}, err
	}
	if !cmd.OK(ctx, "ip link show {}", device) {
		return TargetReceiveResult{}, errTunnelDown(device)
	}
	vmv6 := params.VirtualMachineIPv6

	// The table's sole route: default out the tunnel.
	if _, err := cmd.Run(ctx, "sudo ip -6 route replace default dev {} table {}", device, table); err != nil {
		return TargetReceiveResult{}, err
	}
	// The rule selecting that table for packets sourced from this VM. `ip rule add`
	// STACKS, so guard on the rendered rule already being present.
	existing, _ := cmd.RunUnchecked(ctx, "ip -6 rule show")
	if !strings.Contains(existing, fmt.Sprintf("from %s lookup %d", vmv6, table)) {
		if _, err := cmd.Run(ctx, "sudo ip -6 rule add from {} lookup {} priority 100", vmv6, table); err != nil {
			return TargetReceiveResult{}, err
		}
	}
	return TargetReceiveResult{Receiving: true}, nil
}

// WithdrawPrivate is the source side of a cutover's soft sequencing: withdraw the VM's
// private /128 from THIS (source) host's local-ownership cache, so the source's ANCP
// daemon stops advertising it BEFORE the target boots the guest and advertises the
// SAME /128. Two origins advertising one /128 is the conflict that blackholes the
// VM's private plane for the whole hydration window, so the withdrawal must land
// first. The private /128 is host-independent (a pure HKDF of tenant+VM), so the cache
// entry is the same string on both hosts; only WHICH host advertises it changes.
//
// It touches ONLY the ownership cache — no netns, veth, disk or LV — so it never
// disturbs the source copy the rollback path depends on. Idempotent: a no-op when the
// /128 is already gone, and a clean no-op on an empty address (a tenant-less VM's
// cutover can call it unconditionally). Ports scripts/migration-withdraw-private-source.py.
func WithdrawPrivate(privateAddress string) error {
	return withdrawPrivate(privateAddress, func(address string) error {
		return localownership.Remove(localownership.DefaultPath, address)
	})
}

// withdrawPrivate is the seam the exported form wraps, so a test drives the cache
// mutation through a fake rather than touching /etc.
func withdrawPrivate(privateAddress string, remove func(address string) error) error {
	if privateAddress == "" {
		return nil
	}
	return remove(privateAddress)
}
