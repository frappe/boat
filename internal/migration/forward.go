package migration

import (
	"context"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/run"
)

// The keep-address forward tunnel (spec/19 §2.9): when a VM migrates keeping its
// /128, the source keeps holding the /64 the /128 is carved from, keeps receiving
// the VM's inbound, and forwards it to the target over a per-VM point-to-point
// tunnel. The tunnel is a `tun` device socat bridges to a plain TCP stream (stage-1
// unencrypted); its device, port and table are pure functions of the UUID, so both
// hosts derive them identically and a re-entry needs no stored state.

const (
	// tunnelMTU pins the tun to the IPv6 minimum so the socat/TCP encapsulation never
	// triggers in-tunnel PMTU surprises under live load.
	tunnelMTU = 1280
	// The two ends address the tun with a link-local /64 purely so the device is "up
	// with an address"; the guest's /128 is ROUTED over it, not assigned on it.
	sourceLinkLocal = "fe80::a/64"
	targetLinkLocal = "fe80::b/64"

	roleSource = "source"
	roleTarget = "target"
)

// ForwardUpParams is the tunnel end this host brings up. VirtualMachineIPv6 and
// RouteTable are the traffic-steering state, OPTIONAL because forward-up runs twice:
// at TargetPreparing (bare tunnel — vmv6 empty) and again at cutover (routes now
// known). SourceHost is the address the target's connector dials (target role only).
type ForwardUpParams struct {
	Role               string
	SourceHost         string
	VirtualMachineIPv6 string
}

// ForwardUpResult reports the device and that it is up, for the controller's record.
type ForwardUpResult struct {
	TunnelDevice string
	Up           bool
}

// ForwardUp brings up ONE end of the keep-address forward tunnel. The controller runs
// it on BOTH hosts (source first as the listener, then target as the connector). The
// route installs that make traffic actually flow come later, at cutover
// (SourceForward / TargetReceive). Idempotent: an already-up tun with a live socat is
// left alone; a re-entry re-asserts the address/MTU and only (re)starts socat when it
// is not running, or when new route args must be baked into its restart hook. Ports
// scripts/migration-forward-up.py.
func ForwardUp(ctx context.Context, cmd commands, uuid string, params ForwardUpParams) (ForwardUpResult, error) {
	if params.Role != roleSource && params.Role != roleTarget {
		return ForwardUpResult{}, errInvalidRole(params.Role)
	}
	if params.Role == roleTarget && params.SourceHost == "" {
		return ForwardUpResult{}, errTargetNeedsSourceHost
	}
	device, err := TunnelDevice(uuid)
	if err != nil {
		return ForwardUpResult{}, err
	}
	port, err := TunnelPort(uuid)
	if err != nil {
		return ForwardUpResult{}, err
	}
	table, err := TunnelTable(uuid)
	if err != nil {
		return ForwardUpResult{}, err
	}

	// Forwarding across the tunnel<->veth seam. Set at bootstrap and re-applied at
	// network bring-up; a defensive re-assert here costs nothing and covers a host
	// that came up between bootstrap and this migration.
	cmd.RunUnchecked(ctx, "sudo sysctl -q -w net.ipv6.conf.all.forwarding=1")

	if err := ensureSocat(ctx, cmd, params, device, port, table); err != nil {
		return ForwardUpResult{}, err
	}
	if err := addressTunnel(ctx, cmd, params.Role, device); err != nil {
		return ForwardUpResult{}, err
	}
	return ForwardUpResult{TunnelDevice: device, Up: true}, nil
}

// ensureSocat (re)starts the socat that OWNS the tun device and bridges it to the TCP
// stream. socat creates the tun (tun-name=…), so the device's existence and the
// carrier's liveness are one fact — idempotency keys on the unit.
//
// socat runs as a transient systemd unit's own main process (systemd-run, not a
// daemon child): a bare `&` dies on session close, and — the load-bearing part — a
// keep-address forward must outlive its migration, so Restart=always relaunches socat
// when its single TCP carrier drops (an idle-timeout, a peer reboot, an RST).
// Crucially, a restart makes socat create a BRAND-NEW tun with the addr/MTU/route
// gone, so the full path is re-laid on EVERY start via ExecStartPost (see
// rewireCommand). The desired ExecStartPost GROWS at cutover (route args arrive), so
// the pre-cutover re-entry (bare tunnel, unit already alive) is the only skip; once
// route args are present the unit is always re-laid.
func ensureSocat(ctx context.Context, cmd commands, params ForwardUpParams, device string, port, table int) error {
	unit := forwardUnit(port)
	if socatActive(ctx, cmd, unit) && params.VirtualMachineIPv6 == "" {
		return nil
	}
	// TUN is address 1: socat opens address 1 immediately but address 2 only once 1 is
	// established, so a TCP-LISTEN first would deadlock (the peer's TUN waits the same
	// way). TUN first makes the device appear the instant socat starts, on both ends.
	tun := "TUN,tun-name=" + device + ",iff-up,iff-no-pi"
	// keepalive with a ~30s detection budget so a silently-dead carrier is torn down
	// and redialed inside a human's "it's down" window without flapping on a stall.
	keepalive := "keepalive,keepidle=10,keepintvl=4,keepcnt=5"
	endpoint := fmt.Sprintf("TCP-LISTEN:%d,bind=0.0.0.0,reuseaddr,%s", port, keepalive)
	if params.Role == roleTarget {
		endpoint = fmt.Sprintf("TCP:%s:%d,retry=5,forever,%s", params.SourceHost, port, keepalive)
	}
	// STOP any prior instance (not just reset-failed): a cutover re-lay must replace a
	// running unit's now-stale ExecStartPost, and Restart=always means reset-failed
	// alone would leave the old process running.
	cmd.RunUnchecked(ctx, "sudo systemctl stop {}", unit)
	cmd.RunUnchecked(ctx, "sudo systemctl reset-failed {}", unit)
	_, err := cmd.Run(
		ctx,
		"sudo systemd-run --unit={} --property=Type=simple --property=Restart=always --property=RestartSec=2 --property=ExecStartPost={} -- socat {} {}",
		unit, rewireCommand(params.Role, device, table, params.VirtualMachineIPv6), tun, endpoint,
	)
	return err
}

// rewireCommand is the ExecStartPost that re-establishes this side's full state onto
// the tun socat just (re)created: link-local addr, the pinned MTU, and — once known
// at cutover — the side's traffic route. One `/bin/sh -c` string (systemd runs a
// single ExecStartPost binary; sh lets it wait-for-device then chain idempotent `ip`
// verbs). Every verb is `replace`. Before cutover vmv6 is empty, so only addr+MTU are
// laid — correct, routes must not exist pre-cutover. The `ip` verbs carry no sudo:
// systemd runs the unit as root.
func rewireCommand(role, device string, table int, vmv6 string) string {
	linkLocal := sourceLinkLocal
	if role == roleTarget {
		linkLocal = targetLinkLocal
	}
	steps := []string{
		fmt.Sprintf("for i in $(seq 50); do ip link show %s >/dev/null 2>&1 && break; sleep 0.1; done", device),
		fmt.Sprintf("ip -6 addr replace %s dev %s nodad", linkLocal, device),
		fmt.Sprintf("ip link set %s mtu %d up", device, tunnelMTU),
	}
	if vmv6 != "" {
		if role == roleSource {
			steps = append(steps, fmt.Sprintf("ip -6 route replace %s/128 dev %s", vmv6, device))
		} else {
			steps = append(steps, fmt.Sprintf("ip -6 route replace default dev %s table %d", device, table))
		}
	}
	// run.Quote single-quotes the joined script into one systemd argument, the same
	// shape shlex.quote gives the Python. The outer systemd-run {} quotes it again for
	// the argv split, so socat's ExecStartPost sees exactly `/bin/sh -c '<script>'`.
	return "/bin/sh -c " + run.Quote(strings.Join(steps, "; "))
}

// addressTunnel re-asserts the link-local addr and pinned MTU on the tun (socat
// brought it up; the address + MTU are ours), waiting briefly for socat to create the
// device on a cold start. Idempotent `replace`/`set`, so a re-entry just re-asserts.
func addressTunnel(ctx context.Context, cmd commands, role, device string) error {
	if !waitForDevice(ctx, cmd, device) {
		return errSocatDeviceTimeout(device)
	}
	linkLocal := sourceLinkLocal
	if role == roleTarget {
		linkLocal = targetLinkLocal
	}
	if _, err := cmd.Run(ctx, "sudo ip -6 addr replace {} dev {} nodad", linkLocal, device); err != nil {
		return err
	}
	_, err := cmd.Run(ctx, "sudo ip link set {} mtu {} up", device, tunnelMTU)
	return err
}

// waitForDevice spins up to ~5s for socat to create the tun device on a cold start —
// the same bounded wait the Python makes, a subprocess sleep so nothing here holds a
// timer. A test scripts the device present and the loop breaks on the first look.
func waitForDevice(ctx context.Context, cmd commands, device string) bool {
	for attempt := 0; attempt < 50; attempt++ {
		if cmd.OK(ctx, "ip link show {}", device) {
			return true
		}
		cmd.RunUnchecked(ctx, "sleep 0.1")
	}
	return false
}

// socatActive reports whether the tunnel's transient unit is active. The tun lives
// and dies with that process, so this is also "is the device up?". is-active answers
// with its output, and any non-active state (inactive/failed/unknown) is not active.
func socatActive(ctx context.Context, cmd commands, unit string) bool {
	output, _ := cmd.RunUnchecked(ctx, "sudo systemctl is-active {}", unit)
	return strings.TrimSpace(output) == "active"
}

// forwardUnit is the transient unit name for a tunnel's socat carrier, keyed on its
// port so up/down/liveness all name the same unit with no stored state.
func forwardUnit(port int) string { return fmt.Sprintf("atlas-mig6-%d", port) }

// ForwardDownParams is the tunnel end to tear down. VirtualMachineIPv6 is the /128
// that was being forwarded; the device, port and table are derived.
type ForwardDownParams struct {
	Role               string
	VirtualMachineIPv6 string
}

// ForwardDownResult reports the teardown ran.
type ForwardDownResult struct {
	Down bool
}

// ForwardDown tears down ONE end of the keep-address forward tunnel — the operator's
// Collapse-forward action (the forward is PERMANENT by default; nothing collapses it
// automatically). Best-effort and idempotent throughout, like vm-network-down: a
// missing device, rule, route or nft handle is not an error, so a half-collapsed
// state converges cleanly. Ports scripts/migration-forward-down.py.
//
// The source's proxy-NDP de-assert is UNCONDITIONAL (the mirror of SourceForward's
// unconditional re-assert): the source answered NDP for the /128 on EVERY provider
// while forwarding, so collapse must stop it on every provider. The Python's "0"
// escape hatch is dropped — the field proved the re-assert is always needed (gotcha
// #13), and a bool that silently defaults to "skip" is a black-hole waiting to happen.
func ForwardDown(ctx context.Context, cmd commands, uuid string, params ForwardDownParams) (ForwardDownResult, error) {
	if params.Role != roleSource && params.Role != roleTarget {
		return ForwardDownResult{}, errInvalidRole(params.Role)
	}
	device, err := TunnelDevice(uuid)
	if err != nil {
		return ForwardDownResult{}, err
	}
	port, err := TunnelPort(uuid)
	if err != nil {
		return ForwardDownResult{}, err
	}
	table, err := TunnelTable(uuid)
	if err != nil {
		return ForwardDownResult{}, err
	}

	if params.Role == roleSource {
		collapseForwardSource(ctx, cmd, params.VirtualMachineIPv6, device)
	} else {
		collapseForwardTarget(ctx, cmd, params.VirtualMachineIPv6, device, table)
	}

	// Common: stop the socat carrier (kills the tun with it) and delete the device
	// defensively in case socat already exited but left it behind.
	unit := forwardUnit(port)
	cmd.RunUnchecked(ctx, "sudo systemctl stop {}", unit)
	cmd.RunUnchecked(ctx, "sudo systemctl reset-failed {}", unit)
	cmd.RunUnchecked(ctx, "sudo ip link del {}", device)
	return ForwardDownResult{Down: true}, nil
}

// collapseForwardSource removes the source-forward state: the /128-into-tunnel route,
// the two nft forward rules (by handle), and the proxy-NDP entry.
func collapseForwardSource(ctx context.Context, cmd commands, vmv6, device string) {
	cmd.RunUnchecked(ctx, "sudo ip -6 route del {} dev {}", vmv6+"/128", device)
	// Delete every forward-chain rule mentioning BOTH this VM's /128 and the tunnel —
	// both the oifname and iifname rules — by handle. `nft -a` prints the handle last.
	chain, _ := cmd.RunUnchecked(ctx, "sudo nft -a list chain inet atlas forward")
	for _, line := range strings.Split(chain, "\n") {
		if !strings.Contains(line, vmv6) || !strings.Contains(line, device) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		cmd.RunUnchecked(ctx, "sudo nft delete rule inet atlas forward handle {}", fields[len(fields)-1])
	}
	if uplink := uplinkTolerant(ctx, cmd); uplink != "" {
		cmd.RunUnchecked(ctx, "sudo ip -6 neigh del proxy {} dev {}", vmv6, uplink)
	}
}

// collapseForwardTarget removes the target return-route state: the `from <vmv6>` rule
// and the private table's default route.
func collapseForwardTarget(ctx context.Context, cmd commands, vmv6, device string, table int) {
	cmd.RunUnchecked(ctx, "sudo ip -6 rule del from {} lookup {} priority 100", vmv6, table)
	cmd.RunUnchecked(ctx, "sudo ip -6 route del default dev {} table {}", device, table)
}
