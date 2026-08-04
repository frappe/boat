// The wg-mesh apply pipeline, ported from commands.py (spec §16.4 / §16.5).
//
// The only permitted control-plane apply is the WHOLE-TABLE atomic one:
//
//	wg syncconf wg-mesh <(wg-quick strip /run/atlas-networkd/wg-mesh.conf)   # FIRST
//	wg set wg-mesh private-key /etc/atlas-networkd/wg-private-key listen-port 51820  # LAST
//
// Never an incremental `wg set peer … allowed-ips …`: an incremental apply opens a
// window in which the same /128 sits under two peers (adds peer B before removing
// it from peer A), which breaks the §16.3 non-overlap invariant render.go works to
// preserve. The whole config is replaced in one syncconf.
//
// The syncconf-FIRST-then-set-private-key-LAST order is LOAD-BEARING and proven on
// a real host (atlas host_mesh.py:378): the pushed config carries NO PrivateKey (the
// secret rides its own 0600 file), and `wg syncconf` from a body without a
// PrivateKey CLEARS the interface key — leaving the device unable to handshake and
// every tunnel silently dead. So the key is reasserted AFTER syncconf. assertApply-
// Order fails the build of the script loud if a future edit flips the two, rather
// than letting the flip surface only as a mesh that never handshakes on a live host.
//
// Process substitution needs a shell, so the two-step apply runs through `bash -c`
// as one auto-quoted argv token (the run.Runner {}-hole quotes the whole script), the
// same idiom the predecessor host_mesh._push_wg_mesh used.
package networkd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// WGDevice is the mesh interface name; wgMeshRoute is the whole private plane
	// this host owns out of it (cryptokey routing then delivers each /128).
	WGDevice    = "wg-mesh"
	wgMeshRoute = "fdaa::/16"

	// DefaultRunConfigPath is the tmpfs config the render writes and syncconf reads.
	// /run is tmpfs, so it never persists a reboot — the durable state is the JSON
	// under the state dir; this is the derived, disposable render output.
	DefaultRunConfigPath = "/run/atlas-networkd/wg-mesh.conf"

	// defaultMeshMTU is pinned (proven on real Scaleway hosts): wg adds ~80 B over a
	// 1500-byte path, so a larger frame blackholes without it.
	defaultMeshMTU = 1420
)

// errApplyOrder is the loud refusal assertApplyOrder returns when the two-step apply
// script is not syncconf-then-set-private-key. It is unreachable by construction —
// applyScript assembles the halves in the right order — but the check is the
// construction-time guard commands.py enforced with an `assert`, kept as a returned
// error (no panic in library code, Taste.md) and exercised directly by a test.
var errApplyOrder = errors.New(
	"wg-mesh apply script violates the load-bearing order: `wg syncconf` MUST precede " +
		"`wg set private-key`, else syncconf clears the interface key and kills every tunnel",
)

// syncconfCommand is the whole-table atomic apply: replace the entire peer set from
// the config in one shot. `wg-quick strip` drops the [Interface] private-key line
// (which our config never carries anyway) and keeps every [Peer] verbatim.
func syncconfCommand(configPath, device string) string {
	return fmt.Sprintf("wg syncconf %s <(wg-quick strip %s)", device, configPath)
}

// setKeyCommand asserts the private key (from its 0600 file, never inline) and the
// listen port. MUST run AFTER syncconf — see assertApplyOrder.
func setKeyCommand(port int, keyPath, device string) string {
	return fmt.Sprintf("wg set %s private-key %s listen-port %d", device, keyPath, port)
}

// applyScript builds the two-step hot-path apply body fed to `bash -c`, assuming the
// device already exists and is up. It assembles syncconf FIRST and set-private-key
// LAST and then re-proves that order on the assembled string, returning errApplyOrder
// if it is ever violated.
func applyScript(configPath, keyPath string, port int, device string) (string, error) {
	script := "set -e; " + syncconfCommand(configPath, device) + "; " + setKeyCommand(port, keyPath, device)
	if err := assertApplyOrder(script); err != nil {
		return "", err
	}
	return script, nil
}

// assertApplyOrder proves the load-bearing invariant on the finished script: the
// `syncconf` token precedes the `private-key` token. This is the construction-time
// assertion commands.py documented; a future refactor that reorders the halves is
// caught here (and by TestApplyScriptOrder) rather than on a live host as a mesh
// that never handshakes.
func assertApplyOrder(script string) error {
	syncconfAt := strings.Index(script, "syncconf")
	privateKeyAt := strings.Index(script, "private-key")
	if syncconfAt < 0 || privateKeyAt < 0 || syncconfAt > privateKeyAt {
		return errApplyOrder
	}
	return nil
}

// meshBringUpCommands is the ordered first-boot bring-up (spec §16.5): create the
// device, pin the MTU, assign this host's own infra /128, bring it up, own the
// fdaa::/16 private plane. Each is a discrete `sudo` argv with NO shell — so every one
// is pinnable in sudoers without a wildcard (the sole variable, the mesh address, is
// netip-validated code-side before it is rendered). The create is returned apart
// because its "device already exists" failure is the one expected, ignored outcome;
// every other step is create-or-replace and must fail loud. The peer table is NOT
// applied here — the daemon's first loop tick renders and applies it, so a fresh host
// comes up peer-empty and converges.
func meshBringUpCommands(meshAddress string, mtu int, device string) (create string, steps []string) {
	create = fmt.Sprintf("sudo ip link add dev %s type wireguard", device)
	steps = []string{
		fmt.Sprintf("sudo ip link set dev %s mtu %d", device, mtu),
		fmt.Sprintf("sudo ip -6 addr replace %s/128 dev %s", meshAddress, device),
		fmt.Sprintf("sudo ip link set dev %s up", device),
		fmt.Sprintf("sudo ip -6 route replace %s dev %s", wgMeshRoute, device),
	}
	return create, steps
}

// writeRunConfig atomically writes the rendered wg-mesh.conf via a tempfile + rename,
// so a crash mid-write never leaves a truncated body for `wg syncconf` to choke on.
// The body already carries its own trailing newline (render.go), so none is added.
func writeRunConfig(path, body string) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if _, err := temporary.WriteString(body); err != nil {
		temporary.Close()
		os.Remove(name)
		return err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// runBringUp brings the wg-mesh device up as discrete pinned commands. The device
// create is best-effort — an already-present device returns non-zero, which is the
// idempotent case — while every subsequent step is checked, so a genuine bring-up
// fault (missing wireguard module, denied sudo) fails loud on the very next command.
func runBringUp(ctx context.Context, runner commands, meshAddress string, mtu int) error {
	create, steps := meshBringUpCommands(meshAddress, mtu, WGDevice)
	_, _ = runner.Run(ctx, create) // device-exists is the expected, ignored failure
	for _, step := range steps {
		if _, err := runner.Run(ctx, step); err != nil {
			return err
		}
	}
	return nil
}

// runApply writes the rendered config and runs the whole-table two-step apply through
// `sudo bash -c` — the one shell invocation, needed for the `<(wg-quick strip …)`
// process substitution. Its script is entirely fixed paths + constants, so sudoers
// pins it as a single literal `bash -c` line (byte-coupled to applyScript).
func runApply(ctx context.Context, runner commands, body, configPath, keyPath string, port int) error {
	if err := writeRunConfig(configPath, body); err != nil {
		return err
	}
	script, err := applyScript(configPath, keyPath, port, WGDevice)
	if err != nil {
		return err
	}
	_, err = runner.Run(ctx, "sudo bash -c {}", script)
	return err
}
