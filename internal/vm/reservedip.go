package vm

import (
	"context"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/netapply/reservedip"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/sidecar"
)

// The network.env keys reserved-IP attach reads and writes. The guest's private
// /30 and the host-side veth are provision's, written once and read back here; the
// reserved IP is this verb's own durable record — a cold boot re-creates the 1:1
// NAT from it exactly as an attach did (scripts/vm-network-up.py re-reads it).
const (
	guestCIDRKey    = "IPV4_GUEST_CIDR"
	hostVethKey     = "HOST_VETH"
	reservedIPv4Key = "RESERVED_IPV4"
	networkEnvMode  = "0644"
)

// ReservedIPRequest is a reserved-IP attach or detach. The reserved IP is the
// public v4 to 1:1-NAT to this VM's guest; Detach removes the NAT and the durable
// flag instead, keyed on the guest, so it needs no reserved IP of its own.
type ReservedIPRequest struct {
	ReservedIPv4 string
	Detach       bool
}

// ReservedIP attaches or detaches a Reserved IP's host-side 1:1 NAT to a running
// VM with no reboot, and records it in the VM's network.env so a later cold boot
// re-applies it from disk.
//
// The guest's private v4 and the host veth are the host's own record, read from
// the sidecar rather than taken from the caller — the same argument sleep and
// rebuild make for the per-VM uid: a caller's copy can be stale, and a NAT built
// around the wrong guest address is a reserved IP that lands nowhere. The reserved
// IP itself IS the caller's to state — it is the public identity Atlas allocated —
// and reservedip validates it at the boundary that renders it into an nft rule.
//
// The durable flag is written before the live rules, so a crash between the two
// leaves an env a cold boot re-applies cleanly and a host with no half-state the
// next attach cannot heal. Ported from scripts/vm-reserved-ip.py.
func (manager *Manager) ReservedIP(
	ctx context.Context, runner *run.Runner, uuid string, request ReservedIPRequest,
) (reservedip.Delivery, error) {
	files := manager.filesFor(uuid)
	commands := manager.commandsFor(runner)

	text, err := commands.Run(ctx, "sudo cat {}", files.networkEnvironment)
	if err != nil {
		return reservedip.Delivery{}, err
	}
	guestIPv4, _, _ := strings.Cut(sidecar.Value(text, guestCIDRKey), "/")
	if guestIPv4 == "" {
		return reservedip.Delivery{}, fmt.Errorf("%s names no %s; the VM has no private v4 to NAT to", files.networkEnvironment, guestCIDRKey)
	}
	hostVeth := sidecar.Value(text, hostVethKey)
	if hostVeth == "" {
		return reservedip.Delivery{}, fmt.Errorf("%s names no %s; the inbound flow has no veth to be accepted out", files.networkEnvironment, hostVethKey)
	}

	if request.Detach {
		updated := sidecar.Remove(text, reservedIPv4Key)
		if err := commands.InstallFile(ctx, updated, files.networkEnvironment, networkEnvMode); err != nil {
			return reservedip.Delivery{}, err
		}
		return reservedip.Delivery{}, manager.detachReservedIP(ctx, runner, guestIPv4)
	}

	updated := sidecar.Upsert(text, reservedIPv4Key, request.ReservedIPv4)
	if err := commands.InstallFile(ctx, updated, files.networkEnvironment, networkEnvMode); err != nil {
		return reservedip.Delivery{}, err
	}
	return manager.attachReservedIP(ctx, runner, guestIPv4, hostVeth, request.ReservedIPv4)
}
