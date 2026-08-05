package vmnetwork

import (
	"context"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/sidecar"
)

// The two Task verbs Atlas drives over SSH for the features whose cold-boot half
// this package already replays: `firewall-apply` (scripts/firewall-apply.py) and
// `vm-tunnel` (scripts/vm-tunnel.py). Each is the LIVE half — it changes the host
// now, with no reboot — and each writes the durable sidecar Up() reads back at the
// next cold boot, so the two halves can never disagree about a VM's public
// surface.
//
// They live here rather than in a package of their own because the mechanics are
// already here: applyFirewall/removeFirewall and applyTunnel/removeTunnel are the
// very functions the bring-up calls. A verb adds two things and nothing else — the
// sidecar write, and the one decision a bring-up never makes, which is which
// direction was asked for.

// verbCommands is the wider seam a verb needs. The bring-up only reads and
// renders; a verb also WRITES the sidecar the bring-up will later read, mints a
// key, and feeds a secret to a command on stdin. Declared here rather than widened
// into `commands` because these two functions are the only ones that install
// anything, and a seam is defined by the caller that needs it.
type verbCommands interface {
	commands
	InstallFile(ctx context.Context, content string, destination string, mode string) error
	InstallDirectory(ctx context.Context, destination string, mode string) error
	Input(ctx context.Context, stdin string, template string, parameters ...any) (string, error)
}

var _ verbCommands = (*run.Runner)(nil)

// FirewallParams is firewall-apply.py's FirewallInputs: the VM whose public
// ingress is being restricted, which way, and the allowed proto/port tokens.
//
// An EMPTY Rules with Action "apply" is meaningful and is NOT the same as a clear:
// it denies all public ingress, leaving the VM reachable only over a tunnel. Clear
// reverts the VM to fully public.
type FirewallParams struct {
	VirtualMachine string
	Action         string // "apply" | "clear"
	Rules          []string
}

// Firewall applies or clears one VM's public-ingress firewall, live and with no
// reboot (spec/20). No result line, as the Python has none.
func Firewall(ctx context.Context, cmd verbCommands, params FirewallParams) error {
	virtualMachine, err := virtualMachineFor(params.VirtualMachine)
	if err != nil {
		return fmt.Errorf("firewall-apply: %w", err)
	}
	switch params.Action {
	case "apply":
		return applyFirewallVerb(ctx, cmd, virtualMachine, params.Rules)
	case "clear":
		return clearFirewallVerb(ctx, cmd, virtualMachine)
	}
	return fmt.Errorf("firewall-apply: --action must be apply or clear, got %q", params.Action)
}

// applyFirewallVerb writes the sidecar and installs the nft block.
//
// The sidecar goes down FIRST, matching the Python. A host that dies between the
// two comes back with the firewall applied by the bring-up; the other order comes
// back fully public, which is the failure that cannot be seen from the outside.
func applyFirewallVerb(
	ctx context.Context, cmd verbCommands, virtualMachine paths.VirtualMachine, rules []string,
) error {
	address, err := publicAddress(ctx, cmd, virtualMachine.NetworkEnvironment())
	if err != nil {
		return err
	}
	config := firewallConfig{virtualMachine: address}
	for _, token := range rules {
		rule, err := parseFirewallRule(token)
		if err != nil {
			return err
		}
		config.rules = append(config.rules, rule)
	}
	if err := cmd.InstallFile(ctx, config.environmentText(), virtualMachine.FirewallEnvironment(), "0644"); err != nil {
		return err
	}
	return applyFirewall(ctx, cmd, config)
}

// clearFirewallVerb removes the nft block and the sidecar, reverting the VM to
// fully public.
func clearFirewallVerb(ctx context.Context, cmd verbCommands, virtualMachine paths.VirtualMachine) error {
	address, err := publicAddress(ctx, cmd, virtualMachine.NetworkEnvironment())
	if err != nil {
		return err
	}
	if err := removeFirewall(ctx, cmd, address); err != nil {
		return err
	}
	// Best-effort: a clear can run after a terminate has already rm -rf'd the VM
	// tree, and a sidecar that is not there is exactly the outcome asked for.
	_, err = cmd.RunUnchecked(ctx, "sudo rm -f {}", virtualMachine.FirewallEnvironment())
	return err
}

// TunnelParams is vm-tunnel.py's TunnelInputs. The up-only parameters below the
// action are ignored on down, which works from the interface name alone.
type TunnelParams struct {
	TunnelName      string // the VPN Tunnel UUID — names the .env / .key sidecars
	VirtualMachine  string // the VM UUID — locates the VM dir + network.env
	Interface       string // wg-<id>, derived controller-side from the tunnel name
	Action          string // "up" | "down"
	ListenPort      int
	ClientPublicKey string
	ClientAddress   string // the client's overlay /128, bare
	HostAddress     string // the host end's /127 overlay CIDR
}

// TunnelResult is the one field the controller parses back. The host mints the
// keypair and hands out only the PUBLIC half; the private half never leaves the
// host, which is what makes a tunnel un-impersonable from the control plane.
type TunnelResult struct {
	ServerPublicKey string // empty on down
}

// Tunnel brings one VM's WireGuard tunnel up or down, live and with no reboot
// (spec/19).
func Tunnel(ctx context.Context, cmd verbCommands, params TunnelParams) (TunnelResult, error) {
	virtualMachine, err := virtualMachineFor(params.VirtualMachine)
	if err != nil {
		return TunnelResult{}, fmt.Errorf("vm-tunnel: %w", err)
	}
	// The tunnel name is a path segment of both sidecars, so it is held to the same
	// shape as the VM's — Atlas names a VPN Tunnel with a uuid4, and a name carrying
	// `..` or a `/` would write the .key somewhere else entirely.
	if !paths.IsUUID(params.TunnelName) {
		return TunnelResult{}, fmt.Errorf("vm-tunnel: %q is not a tunnel UUID", params.TunnelName)
	}
	switch params.Action {
	case "up":
		return tunnelUp(ctx, cmd, virtualMachine, params)
	case "down":
		return TunnelResult{}, tunnelDown(ctx, cmd, virtualMachine, params)
	}
	return TunnelResult{}, fmt.Errorf("vm-tunnel: --action must be up or down, got %q", params.Action)
}

// tunnelUp mints or reuses the host key, writes both sidecars, and applies the
// live interface plus its isolation rules.
func tunnelUp(
	ctx context.Context, cmd verbCommands, virtualMachine paths.VirtualMachine, params TunnelParams,
) (TunnelResult, error) {
	address, err := publicAddress(ctx, cmd, virtualMachine.NetworkEnvironment())
	if err != nil {
		return TunnelResult{}, err
	}
	keyPath := virtualMachine.TunnelKey(params.TunnelName)
	// 0700: the directory holds a private key, so it is created with an explicit
	// mode rather than left at the umask's mercy.
	if err := cmd.InstallDirectory(ctx, virtualMachine.TunnelsDirectory(), "0700"); err != nil {
		return TunnelResult{}, err
	}
	publicKey, err := ensureHostKey(ctx, cmd, keyPath)
	if err != nil {
		return TunnelResult{}, err
	}
	text := tunnelConfig{
		interfaceName:   params.Interface,
		listenPort:      params.ListenPort,
		privateKeyPath:  keyPath,
		clientPublicKey: params.ClientPublicKey,
		clientAddress:   params.ClientAddress,
		hostAddress:     params.HostAddress,
		virtualMachine:  address,
	}.environmentText()
	// Rendered, then parsed BACK before anything is installed, and that round trip
	// is the validation. parseTunnelConfig is the same reader applyPersistedTunnels
	// replays the sidecar with, so a tunnel this verb accepts but the next cold boot
	// would refuse cannot exist — the VM would come up without it and nobody would
	// learn why. Every field here crosses from the controller into `wg set`, `ip -6
	// addr` and an nft rule, where nft re-lexes a `;` even out of a value run.Quote
	// has already made one shell token.
	config, err := parseTunnelConfig(text)
	if err != nil {
		return TunnelResult{}, err
	}
	if err := cmd.InstallFile(ctx, text, virtualMachine.TunnelEnvironment(params.TunnelName), "0644"); err != nil {
		return TunnelResult{}, err
	}
	if err := applyTunnel(ctx, cmd, config); err != nil {
		return TunnelResult{}, err
	}
	return TunnelResult{ServerPublicKey: publicKey}, nil
}

// tunnelDown removes the live interface with its rules and deletes the sidecars.
func tunnelDown(
	ctx context.Context, cmd verbCommands, virtualMachine paths.VirtualMachine, params TunnelParams,
) error {
	if !validName(params.Interface) {
		return fmt.Errorf("vm-tunnel: --interface %q is not a usable interface name", params.Interface)
	}
	if err := removeTunnel(ctx, cmd, params.Interface); err != nil {
		return err
	}
	// Best-effort — a revoke may run after a terminate already rm -rf'd the VM tree.
	for _, path := range []string{
		virtualMachine.TunnelEnvironment(params.TunnelName),
		virtualMachine.TunnelKey(params.TunnelName),
	} {
		if _, err := cmd.RunUnchecked(ctx, "sudo rm -f {}", path); err != nil {
			return err
		}
	}
	return nil
}

// ensureHostKey returns the tunnel's host PUBLIC key, minting the private key on
// first use.
//
// Idempotent, and that is the point rather than a bonus: an existing key file is
// REUSED, so a re-apply — a retry, a reconcile — never rotates the key out from
// under a client that already holds the config. `wg pubkey` derives the public
// half from the private, and neither command needs root.
//
// The private key reaches `wg pubkey` on STDIN and `wg set` by PATH, never as an
// argument: a command line is readable off the process table by every local user
// and is echoed into the operation record.
func ensureHostKey(ctx context.Context, cmd verbCommands, keyPath string) (string, error) {
	privateKey := ""
	if cmd.OK(ctx, "sudo test -f {}", keyPath) {
		existing, err := cmd.Run(ctx, "sudo cat {}", keyPath)
		if err != nil {
			return "", err
		}
		privateKey = strings.TrimSpace(existing)
	} else {
		minted, err := cmd.Run(ctx, "wg genkey")
		if err != nil {
			return "", err
		}
		privateKey = strings.TrimSpace(minted)
		if err := cmd.InstallFile(ctx, privateKey+"\n", keyPath, "0600"); err != nil {
			return "", err
		}
	}
	public, err := cmd.Input(ctx, privateKey+"\n", "wg pubkey")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(public), nil
}

// publicAddress reads the VM's public /128 out of the sidecar provision wrote.
//
// Read rather than taken from the controller's flags for the reason sidecar.go
// gives for every VM fact: the file is the host's own record of what this UUID
// owns, and a firewall or a tunnel scoped to an address the controller believed
// but the host never assigned protects nothing.
func publicAddress(ctx context.Context, cmd commands, networkEnvironment string) (string, error) {
	text, err := cmd.Run(ctx, "sudo cat {}", networkEnvironment)
	if err != nil {
		return "", err
	}
	address, ok := canonicalIPv6(sidecar.Value(text, virtualMachineKey))
	if !ok {
		return "", fmt.Errorf("%s: %s is not a canonical IPv6 address", networkEnvironment, virtualMachineKey)
	}
	return address, nil
}

// virtualMachineFor is a VM's path set, refusing a name that is not a UUID.
//
// The name becomes a path segment of every file these verbs write and read, and
// `*` in a sudoers argument matches `/` and `.` and spaces — so a name carrying
// `..` walks out of /var/lib/atlas. That is the guard paths.IsUUID exists for, and
// it runs before the name is rendered anywhere.
func virtualMachineFor(uuid string) (paths.VirtualMachine, error) {
	if !paths.IsUUID(uuid) {
		return paths.VirtualMachine{}, fmt.Errorf("%q is not a VM UUID", uuid)
	}
	return paths.ForVirtualMachine(uuid), nil
}
