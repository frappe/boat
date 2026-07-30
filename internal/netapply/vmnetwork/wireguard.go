package vmnetwork

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/sidecar"
)

// A VM's WireGuard VPN-broker tunnels (spec/19). Each terminates on its own wg
// interface in the host root netns and routes a decrypted packet destined to the
// VM's /128 into that VM's namespace over the route the bring-up already laid.
// Isolation is interface-keyed nft: a forward accept for the VM plus a forward
// drop for everything else off that interface (transit), and an input drop so a
// client cannot reach a host-local service — split across two hooks because the
// kernel routes transit and host-local traffic on different paths.
//
// Ported from scripts/lib/atlas/wireguard.py. The private key is read from a 0600
// file path, never inlined, so no secret crosses a command line. Re-applied at
// bring-up (step 9); a plain teardown leaves the interfaces, exactly as the Python
// does — a revoke verb removes one, which is a separate port.
const (
	forwardChain = "forward"
	inputChain   = "input"
)

// tunnelConfig is one tunnel's durable host state, read from a <tunnel>.env under
// the VM's tunnels/ directory.
type tunnelConfig struct {
	interfaceName   string
	listenPort      int
	privateKeyPath  string
	clientPublicKey string
	clientAddress   string // the client's bare overlay v6; its allowed-ip
	hostAddress     string // the host end's /127 overlay CIDR
	virtualMachine  string // the VM's /128, bare — the one destination the tunnel may reach
}

func parseTunnelConfig(text string) (tunnelConfig, error) {
	value := func(key string) string { return sidecar.Value(text, key) }

	interfaceName := value("INTERFACE")
	if !validName(interfaceName) {
		return tunnelConfig{}, fmt.Errorf("tunnel: INTERFACE=%q is not a usable interface name", interfaceName)
	}
	listenPort, err := strconv.Atoi(value("LISTEN_PORT"))
	if err != nil || listenPort < 1 || listenPort > 65535 {
		return tunnelConfig{}, fmt.Errorf("tunnel: LISTEN_PORT=%q is not a port", value("LISTEN_PORT"))
	}
	virtualMachine, ok := canonicalIPv6(value("VIRTUAL_MACHINE_IPV6"))
	if !ok {
		return tunnelConfig{}, fmt.Errorf("tunnel: VIRTUAL_MACHINE_IPV6=%q is not a canonical IPv6 address", value("VIRTUAL_MACHINE_IPV6"))
	}
	clientAddress, ok := canonicalIPv6(value("CLIENT_ADDRESS"))
	if !ok {
		return tunnelConfig{}, fmt.Errorf("tunnel: CLIENT_ADDRESS=%q is not a canonical IPv6 address", value("CLIENT_ADDRESS"))
	}
	hostAddress, ok := canonicalIPv6Prefix(value("HOST_ADDRESS"))
	if !ok {
		return tunnelConfig{}, fmt.Errorf("tunnel: HOST_ADDRESS=%q is not an IPv6 CIDR", value("HOST_ADDRESS"))
	}
	privateKeyPath := value("PRIVATE_KEY_FILE")
	if privateKeyPath == "" || !validPath(privateKeyPath) {
		return tunnelConfig{}, fmt.Errorf("tunnel: PRIVATE_KEY_FILE=%q is not a usable path", privateKeyPath)
	}
	clientPublicKey := value("CLIENT_PUBLIC_KEY")
	if !validWireGuardKey(clientPublicKey) {
		return tunnelConfig{}, fmt.Errorf("tunnel: CLIENT_PUBLIC_KEY is not a WireGuard key")
	}
	return tunnelConfig{
		interfaceName:   interfaceName,
		listenPort:      listenPort,
		privateKeyPath:  privateKeyPath,
		clientPublicKey: clientPublicKey,
		clientAddress:   clientAddress,
		hostAddress:     hostAddress,
		virtualMachine:  virtualMachine,
	}, nil
}

// applyPersistedTunnels re-applies every tunnel under the VM's tunnels/ directory
// at bring-up, in sorted order (matching the Python's sorted(os.listdir)). No
// directory (a VM with no tunnels) is a no-op.
func applyPersistedTunnels(ctx context.Context, commands commands, tunnelsDirectory string) error {
	if !commands.OK(ctx, "sudo test -d {}", tunnelsDirectory) {
		return nil
	}
	listing, err := commands.Run(ctx, "sudo ls -1 {}", tunnelsDirectory)
	if err != nil {
		return err
	}
	var entries []string
	for _, entry := range strings.Fields(listing) {
		if strings.HasSuffix(entry, ".env") {
			entries = append(entries, entry)
		}
	}
	sort.Strings(entries)
	for _, entry := range entries {
		text, err := commands.Run(ctx, "sudo cat {}", tunnelsDirectory+"/"+entry)
		if err != nil {
			return err
		}
		config, err := parseTunnelConfig(text)
		if err != nil {
			return err
		}
		if err := applyTunnel(ctx, commands, config); err != nil {
			return err
		}
	}
	return nil
}

// applyTunnel brings one tunnel up idempotently: the interface, its key/port and
// peer, the host overlay address, and the isolation rules. Re-running is a no-op.
func applyTunnel(ctx context.Context, commands commands, config tunnelConfig) error {
	if !commands.OK(ctx, "sudo ip link show {}", config.interfaceName) {
		if _, err := commands.Run(ctx, "sudo "+linkAddCommand(config.interfaceName)); err != nil {
			return err
		}
	}
	if _, err := commands.Run(ctx, "sudo "+wgSetInterfaceCommand(config.interfaceName, config.listenPort, config.privateKeyPath)); err != nil {
		return err
	}
	if _, err := commands.Run(ctx, "sudo "+wgSetPeerCommand(config.interfaceName, config.clientPublicKey, config.clientAddress)); err != nil {
		return err
	}
	addresses, err := commands.RunUnchecked(ctx, "sudo ip -6 addr show dev {}", config.interfaceName)
	if err != nil {
		return err
	}
	if !strings.Contains(addresses, config.hostAddress) {
		if _, err := commands.Run(ctx, "sudo "+addrAddCommand(config.interfaceName, config.hostAddress)); err != nil {
			return err
		}
	}
	if _, err := commands.Run(ctx, "sudo "+linkUpCommand(config.interfaceName)); err != nil {
		return err
	}

	if err := ensureChain(ctx, commands, forwardChain, forwardChainSpecification); err != nil {
		return err
	}
	forward, err := commands.Run(ctx, "sudo nft list chain inet atlas {}", forwardChain)
	if err != nil {
		return err
	}
	// Insert drop FIRST, accept SECOND, so the head ends [accept, drop, …per-VM…].
	if !tunnelHasDrop(forward, config.interfaceName) {
		if _, err := commands.Run(ctx, "sudo nft "+tunnelDropCommand(config.interfaceName)); err != nil {
			return err
		}
	}
	if !tunnelHasAccept(forward, config.interfaceName, config.virtualMachine) {
		if _, err := commands.Run(ctx, "sudo nft "+tunnelAcceptCommand(config.interfaceName, config.virtualMachine)); err != nil {
			return err
		}
	}

	if err := ensureChain(ctx, commands, inputChain, inputChainSpecification); err != nil {
		return err
	}
	hostInput, err := commands.Run(ctx, "sudo nft list chain inet atlas {}", inputChain)
	if err != nil {
		return err
	}
	if !tunnelHasDrop(hostInput, config.interfaceName) {
		if _, err := commands.Run(ctx, "sudo nft "+tunnelHostDropCommand(config.interfaceName)); err != nil {
			return err
		}
	}
	return nil
}

// ensureChain creates a base chain on demand, guarded — the forward chain is the
// bring-up scaffold's, but a tunnel apply must be self-sufficient, and the input
// chain is this module's own.
func ensureChain(ctx context.Context, commands commands, chain, specification string) error {
	if commands.OK(ctx, "sudo nft list chain inet atlas {}", chain) {
		return nil
	}
	_, err := commands.Run(ctx, "sudo nft add chain inet atlas {} {}", chain, specification)
	return err
}

const inputChainSpecification = "{ type filter hook input priority filter; policy accept; }"

// --- command builders (values run.Quote'd) ---

func linkAddCommand(interfaceName string) string {
	return "ip link add " + run.Quote(interfaceName) + " type wireguard"
}

func linkUpCommand(interfaceName string) string {
	return "ip link set " + run.Quote(interfaceName) + " up"
}

func addrAddCommand(interfaceName, hostCIDR string) string {
	return fmt.Sprintf("ip -6 addr add %s dev %s", run.Quote(hostCIDR), run.Quote(interfaceName))
}

func wgSetInterfaceCommand(interfaceName string, listenPort int, privateKeyPath string) string {
	return fmt.Sprintf("wg set %s listen-port %s private-key %s",
		run.Quote(interfaceName), strconv.Itoa(listenPort), run.Quote(privateKeyPath))
}

func wgSetPeerCommand(interfaceName, clientPublicKey, clientAddress string) string {
	return fmt.Sprintf("wg set %s peer %s allowed-ips %s",
		run.Quote(interfaceName), run.Quote(clientPublicKey), run.Quote(clientAddress+"/128"))
}

func tunnelAcceptCommand(interfaceName, virtualMachine string) string {
	return fmt.Sprintf("insert rule inet atlas %s iifname %s ip6 daddr %s accept",
		forwardChain, run.Quote(interfaceName), run.Quote(virtualMachine))
}

func tunnelDropCommand(interfaceName string) string {
	return fmt.Sprintf("insert rule inet atlas %s iifname %s drop", forwardChain, run.Quote(interfaceName))
}

func tunnelHostDropCommand(interfaceName string) string {
	return fmt.Sprintf("add rule inet atlas %s iifname %s drop", inputChain, run.Quote(interfaceName))
}

func tunnelHasAccept(listing, interfaceName, virtualMachine string) bool {
	return anyLine(listing, func(line string) bool {
		return strings.Contains(line, interfaceName) && strings.Contains(line, virtualMachine) && strings.Contains(line, "accept")
	})
}

func tunnelHasDrop(listing, interfaceName string) bool {
	return anyLine(listing, func(line string) bool {
		return strings.Contains(line, interfaceName) && strings.Contains(line, "drop")
	})
}

// validPath admits a plausible absolute path and nothing that would break out of
// a command argument. The private-key path is host-written, but it reaches `wg
// set private-key <path>`, so it is checked like every other value.
func validPath(path string) bool {
	if !strings.HasPrefix(path, "/") || len(path) > 4096 {
		return false
	}
	for index := 0; index < len(path); index++ {
		character := path[index]
		letter := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
		digit := character >= '0' && character <= '9'
		punctuation := strings.IndexByte("/._-@", character) >= 0
		if !letter && !digit && !punctuation {
			return false
		}
	}
	return true
}

// validWireGuardKey admits only a base64 WireGuard key (44 chars, standard
// alphabet). It reaches `wg set peer <key>`, so an unparseable value is refused.
func validWireGuardKey(key string) bool {
	if len(key) != 44 || !strings.HasSuffix(key, "=") {
		return false
	}
	for index := 0; index < len(key)-1; index++ {
		character := key[index]
		if strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", character) < 0 {
			return false
		}
	}
	return true
}
