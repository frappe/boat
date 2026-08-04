// The key material on disk, ported from keys.py (spec §7.1 / §8 / §19.3).
//
// Two keypairs live under /etc/atlas-networkd/:
//
//   - The WireGuard keypair (wg-private-key 0600 / wg-public-key 0644), base64
//     because that is what `wg set private-key` reads. In the normal path the
//     controller derives and pushes both at bootstrap and EnsureWireGuardKeypair
//     adopts them verbatim; only when the files are ABSENT does it self-generate a
//     fresh pair via `wg genkey` + `wg pubkey` — the one host touch in this package,
//     routed through the run seam so a test drives it with a recorder and no wg.
//   - The ed25519 signing keypair (signing-private-key 0600 / signing-public-key
//     0644), generated with the standard library, never derived (a derived signing
//     key's seed would be public, defeating it).
//
// Both ensures are idempotent: a valid existing pair is adopted, an absent or
// invalid one is regenerated. "Valid" is re-derived, not assumed — the wg public is
// re-checked against its private through `wg pubkey`, and the signing public is
// re-checked by signing and verifying a nonce, so a tampered or half-written pair is
// regenerated rather than trusted.
package networkd

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/frappe/boat/internal/run"
)

const (
	// DefaultWireGuardPrivateKeyPath and its siblings are the fixed key layout the
	// systemd unit and the controller push both name literally.
	DefaultWireGuardPrivateKeyPath = "/etc/atlas-networkd/wg-private-key"
	DefaultWireGuardPublicKeyPath  = "/etc/atlas-networkd/wg-public-key"
	DefaultSigningPrivateKeyPath   = "/etc/atlas-networkd/signing-private-key"
	DefaultSigningPublicKeyPath    = "/etc/atlas-networkd/signing-public-key"
)

// commands is the run seam this package needs — `wg genkey` captures its stdout and
// `wg pubkey` reads the private key on stdin. Outside tests there is one
// implementation, *run.Runner; a test supplies a recorder so the whole keygen path
// runs with no wg and no root.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	Input(ctx context.Context, stdin string, template string, parameters ...any) (string, error)
}

var _ commands = (*run.Runner)(nil)

// EnsureWireGuardKeypair idempotently ensures a WireGuard keypair exists at the two
// paths, returning (private_b64, public_b64). A valid existing pair is adopted (the
// controller's pushed pair, so a re-bootstrap re-derives the SAME identity with zero
// peer churn); otherwise a fresh pair is generated and written. Fails loud on a
// `wg genkey`/`wg pubkey` error (a host missing wireguard-tools, an entropy fault) —
// never falls back to a derived or weak key.
func EnsureWireGuardKeypair(ctx context.Context, runner *run.Runner, privateKeyPath, publicKeyPath string) (string, string, error) {
	return ensureWireGuardKeypair(ctx, runner, privateKeyPath, publicKeyPath)
}

func ensureWireGuardKeypair(ctx context.Context, commands commands, privateKeyPath, publicKeyPath string) (string, string, error) {
	if existingWireGuardPairValid(ctx, commands, privateKeyPath, publicKeyPath) {
		return readKey(privateKeyPath), readKey(publicKeyPath), nil
	}
	private, err := commands.Run(ctx, "wg genkey")
	if err != nil {
		return "", "", err
	}
	private = strings.TrimSpace(private)
	public, err := deriveWireGuardPublic(ctx, commands, private)
	if err != nil {
		return "", "", err
	}
	if err := writeKey(privateKeyPath, private, 0o600); err != nil {
		return "", "", err
	}
	if err := writeKey(publicKeyPath, public, 0o644); err != nil {
		return "", "", err
	}
	return private, public, nil
}

// existingWireGuardPairValid reports whether both files exist and the stored public
// is the legitimate mate of the stored private, re-derived through `wg pubkey` so a
// tampered or interrupted first-boot pair is regenerated rather than trusted.
func existingWireGuardPairValid(ctx context.Context, commands commands, privatePath, publicPath string) bool {
	if !fileExists(privatePath) || !fileExists(publicPath) {
		return false
	}
	private := readKey(privatePath)
	if !validWireGuardPrivate(private) {
		return false
	}
	derived, err := deriveWireGuardPublic(ctx, commands, private)
	if err != nil {
		return false
	}
	return derived == readKey(publicPath)
}

// deriveWireGuardPublic pipes the private key into `wg pubkey`. The private crosses
// as stdin, never an argv, so it never reaches the process table or the trace.
func deriveWireGuardPublic(ctx context.Context, commands commands, private string) (string, error) {
	public, err := commands.Input(ctx, private+"\n", "wg pubkey")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(public), nil
}

// validWireGuardPrivate is the cheap sanity check keys.py used: 32 bytes base64 is
// 44 characters ending with one `=` pad. The real validation is the `wg pubkey`
// round trip in existingWireGuardPairValid.
func validWireGuardPrivate(value string) bool {
	if len(value) != 44 || !strings.HasSuffix(value, "=") {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		letter := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
		digit := character >= '0' && character <= '9'
		if !letter && !digit && character != '+' && character != '/' && character != '=' {
			return false
		}
	}
	return true
}

// EnsureSigningKeypair idempotently ensures an ed25519 signing keypair exists at the
// two paths, returning (private_b64, public_b64). Same 0600/0644 posture as the wg
// keypair. No host touch — the standard library mints the key. A valid existing pair
// is adopted; an absent or invalid one is regenerated.
func EnsureSigningKeypair(signingPrivateKeyPath, signingPublicKeyPath string) (string, string, error) {
	if existingSigningPairValid(signingPrivateKeyPath, signingPublicKeyPath) {
		return readKey(signingPrivateKeyPath), readKey(signingPublicKeyPath), nil
	}
	private, public, err := GenerateSigningKeypair()
	if err != nil {
		return "", "", err
	}
	if err := writeKey(signingPrivateKeyPath, private, 0o600); err != nil {
		return "", "", err
	}
	if err := writeKey(signingPublicKeyPath, public, 0o644); err != nil {
		return "", "", err
	}
	return private, public, nil
}

// existingSigningPairValid reports whether both signing key files exist AND the
// public verifies against the private, by signing a nonce and verifying it — a
// tampered or interrupted pair is regenerated rather than trusted.
//
// CANARY: when BOTH files exist with non-empty content but the pair does not verify,
// it logs a loud warning before returning false. The most common cause is the
// controller pushing the Frappe Password-field mask ("****") instead of the
// decrypted plaintext to signing-private-key; silent regeneration then leaves the
// host with a key the controller does not know, and the cluster silently partitions
// on the next signed record. Without this canary that bug class is invisible from
// the host side — it left the on-disk public diverged from the controller's record
// for weeks.
func existingSigningPairValid(privatePath, publicPath string) bool {
	if !fileExists(privatePath) || !fileExists(publicPath) {
		return false
	}
	private := readKey(privatePath)
	public := readKey(publicPath)
	nonce := map[string]any{"v": "self-test", "host_id": "self", "generation": Generation(0)}
	signature, err := sign(nonce, kindMembership, private)
	if err == nil {
		err = verify(nonce, signature, kindMembership, public)
	}
	if err == nil {
		return true
	}
	if private != "" && public != "" {
		slog.Warn(
			"atlas-networkd: existing signing keypair failed validation; regenerating a fresh keypair. "+
				"If the controller's Server.signing_public_key for this host does not match the new on-disk "+
				"value, the host and controller have diverged and the cluster will silently partition on every "+
				"signed-record verify. Most common cause: the controller pushed the Frappe Password-field mask "+
				"(\"****\") instead of the decrypted plaintext to signing-private-key.",
			"private_key_path", privatePath, "public_key_path", publicPath, "error", err,
		)
	}
	return false
}

// fileExists is a presence check; anything but a clean stat (missing, or a directory
// the reader cannot enter) reads as absent, matching keys.py's Path.exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// readKey reads a key file and strips surrounding whitespace — none is expected, but
// a stray trailing newline must not corrupt the base64.
func readKey(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeKey writes content atomically via a tempfile + rename, chmod'ing the final
// path, creating /etc/atlas-networkd if missing. A crash mid-write leaves the
// previous key intact rather than a half-written one.
func writeKey(path, content string, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if _, err := temporary.WriteString(content + "\n"); err != nil {
		temporary.Close()
		os.Remove(name)
		return err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
