// The recorder every host-touching test in this package runs on, plus the keys
// tests. keys.go is the only file that shells out (`wg genkey` / `wg pubkey`), so it
// is the only one that needs the fake; the golden command lines are written out as
// literal strings so a template that drifts from keys.py's shows up as a failing
// test rather than a host that quietly stops generating keys.

package networkd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errFakeCommandFailed = errors.New("command failed")

// Two distinct 44-character base64 strings (43 body chars + one `=` pad, the shape a
// 32-byte WireGuard key takes). The fake returns them; no real wg runs.
const (
	testWireGuardPrivate = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	testWireGuardPublic  = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB="
)

// fakeCommands answers `wg genkey` / `wg pubkey` from a script and records every
// command it was asked to run, plus the stdin each Input received.
type fakeCommands struct {
	outputs map[string]string
	failing map[string]bool
	trace   []string
	stdins  []string
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{outputs: map[string]string{}, failing: map[string]bool{}}
}

func (fake *fakeCommands) Run(_ context.Context, template string, parameters ...any) (string, error) {
	command := renderCommand(template, parameters...)
	fake.trace = append(fake.trace, command)
	if fake.failing[command] {
		return "", errFakeCommandFailed
	}
	return fake.outputs[command], nil
}

func (fake *fakeCommands) Input(_ context.Context, stdin, template string, parameters ...any) (string, error) {
	command := renderCommand(template, parameters...)
	fake.trace = append(fake.trace, command)
	fake.stdins = append(fake.stdins, stdin)
	if fake.failing[command] {
		return "", errFakeCommandFailed
	}
	return fake.outputs[command], nil
}

// renderCommand substitutes each {} with its parameter unquoted — every template in
// this package is a literal, so this is mostly an identity, and it panics on an
// arity mismatch to catch a miscounted template for free.
func renderCommand(template string, parameters ...any) string {
	parts := strings.Split(template, "{}")
	if len(parts)-1 != len(parameters) {
		panic(fmt.Sprintf("%q: %d placeholders, %d parameters", template, len(parts)-1, len(parameters)))
	}
	var builder strings.Builder
	for index, part := range parts {
		builder.WriteString(part)
		if index < len(parameters) {
			fmt.Fprintf(&builder, "%v", parameters[index])
		}
	}
	return builder.String()
}

// captureJournal swaps the default logger for one writing into a buffer, so a test
// can assert what the canary did — and did not — say.
func captureJournal(t *testing.T) *bytes.Buffer {
	t.Helper()
	journal := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(journal, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return journal
}

// A fresh host with no key files generates a pair: `wg genkey` then `wg pubkey` with
// the private on stdin, and both files land on disk.
func TestEnsureWireGuardKeypairGeneratesWhenAbsent(t *testing.T) {
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "wg-private-key")
	publicPath := filepath.Join(directory, "wg-public-key")

	fake := newFakeCommands()
	fake.outputs["wg genkey"] = testWireGuardPrivate + "\n"
	fake.outputs["wg pubkey"] = testWireGuardPublic + "\n"

	private, public, err := ensureWireGuardKeypair(context.Background(), fake, privatePath, publicPath)
	if err != nil {
		t.Fatalf("ensureWireGuardKeypair: %v", err)
	}
	if private != testWireGuardPrivate || public != testWireGuardPublic {
		t.Fatalf("returned (%q, %q), want (%q, %q)", private, public, testWireGuardPrivate, testWireGuardPublic)
	}
	assertCommandTrace(t, fake, "wg genkey", "wg pubkey")
	if len(fake.stdins) != 1 || fake.stdins[0] != testWireGuardPrivate+"\n" {
		t.Fatalf("wg pubkey stdin = %q, want the private key with a trailing newline", fake.stdins)
	}
	if got := readKey(privatePath); got != testWireGuardPrivate {
		t.Errorf("private key file = %q, want %q", got, testWireGuardPrivate)
	}
	if got := readKey(publicPath); got != testWireGuardPublic {
		t.Errorf("public key file = %q, want %q", got, testWireGuardPublic)
	}
	if info, _ := os.Stat(privatePath); info.Mode().Perm() != 0o600 {
		t.Errorf("private key mode = %v, want 0600", info.Mode().Perm())
	}
}

// A host the controller already pushed a valid pair to adopts it verbatim: only
// `wg pubkey` runs to re-verify the mate, never `wg genkey`.
func TestEnsureWireGuardKeypairAdoptsValidExistingPair(t *testing.T) {
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "wg-private-key")
	publicPath := filepath.Join(directory, "wg-public-key")
	if err := writeKey(privatePath, testWireGuardPrivate, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeKey(publicPath, testWireGuardPublic, 0o644); err != nil {
		t.Fatal(err)
	}
	fake := newFakeCommands()
	fake.outputs["wg pubkey"] = testWireGuardPublic + "\n"

	private, public, err := ensureWireGuardKeypair(context.Background(), fake, privatePath, publicPath)
	if err != nil {
		t.Fatalf("ensureWireGuardKeypair: %v", err)
	}
	if private != testWireGuardPrivate || public != testWireGuardPublic {
		t.Fatalf("returned (%q, %q), want the adopted pair", private, public)
	}
	assertCommandTrace(t, fake, "wg pubkey")
}

// A pushed pair whose public does not match the private is regenerated, not trusted.
func TestEnsureWireGuardKeypairRegeneratesMismatchedPair(t *testing.T) {
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "wg-private-key")
	publicPath := filepath.Join(directory, "wg-public-key")
	if err := writeKey(privatePath, testWireGuardPrivate, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeKey(publicPath, testWireGuardPublic, 0o644); err != nil {
		t.Fatal(err)
	}
	fake := newFakeCommands()
	// The mate of the STORED private is not the stored public, so validation fails
	// and a fresh pair is generated.
	fake.outputs["wg pubkey"] = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC=\n"
	fake.outputs["wg genkey"] = testWireGuardPrivate + "\n"

	if _, _, err := ensureWireGuardKeypair(context.Background(), fake, privatePath, publicPath); err != nil {
		t.Fatalf("ensureWireGuardKeypair: %v", err)
	}
	if !fake.issued("wg genkey") {
		t.Fatalf("a mismatched pair should have been regenerated, trace: %v", fake.trace)
	}
}

// A `wg genkey` failure aborts loud — no fallback to a derived or weak key.
func TestEnsureWireGuardKeypairFailsLoudOnGenkeyError(t *testing.T) {
	directory := t.TempDir()
	fake := newFakeCommands()
	fake.failing["wg genkey"] = true
	_, _, err := ensureWireGuardKeypair(context.Background(), fake,
		filepath.Join(directory, "priv"), filepath.Join(directory, "pub"))
	if err == nil {
		t.Fatal("expected an error when wg genkey fails")
	}
}

// The ed25519 signing keypair generates when absent, is adopted idempotently on the
// next call, and the generated pair actually signs and verifies.
func TestEnsureSigningKeypairGeneratesThenAdopts(t *testing.T) {
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "signing-private-key")
	publicPath := filepath.Join(directory, "signing-public-key")

	private, public, err := EnsureSigningKeypair(privatePath, publicPath)
	if err != nil {
		t.Fatalf("EnsureSigningKeypair: %v", err)
	}
	if private == "" || public == "" {
		t.Fatal("EnsureSigningKeypair returned an empty key")
	}
	nonce := map[string]any{"probe": "value"}
	signature, err := sign(nonce, kindMembership, private)
	if err != nil {
		t.Fatalf("sign with generated key: %v", err)
	}
	if err := verify(nonce, signature, kindMembership, public); err != nil {
		t.Fatalf("verify with generated key: %v", err)
	}

	again, againPublic, err := EnsureSigningKeypair(privatePath, publicPath)
	if err != nil {
		t.Fatalf("second EnsureSigningKeypair: %v", err)
	}
	if again != private || againPublic != public {
		t.Fatal("a valid existing signing pair should have been adopted unchanged")
	}
}

// The divergence canary: both files present and non-empty but the pair does not
// verify (the controller pushed the "****" mask) regenerates AND logs the loud
// warning that names the silent-partition failure mode.
func TestEnsureSigningKeypairCanaryWarnsOnMaskedKey(t *testing.T) {
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "signing-private-key")
	publicPath := filepath.Join(directory, "signing-public-key")
	if err := writeKey(privatePath, "****", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeKey(publicPath, testWireGuardPublic, 0o644); err != nil {
		t.Fatal(err)
	}
	journal := captureJournal(t)

	if _, _, err := EnsureSigningKeypair(privatePath, publicPath); err != nil {
		t.Fatalf("EnsureSigningKeypair: %v", err)
	}
	if !strings.Contains(journal.String(), "silently partition") {
		t.Fatalf("canary did not warn about the silent partition; journal: %s", journal.String())
	}
	// And it regenerated a working pair.
	if !existingSigningPairValid(privatePath, publicPath) {
		t.Fatal("expected a fresh, valid signing pair after the canary regenerated")
	}
}

func (fake *fakeCommands) issued(fragment string) bool {
	for _, recorded := range fake.trace {
		if strings.Contains(recorded, fragment) {
			return true
		}
	}
	return false
}

func assertCommandTrace(t *testing.T, fake *fakeCommands, expected ...string) {
	t.Helper()
	if len(fake.trace) != len(expected) {
		t.Fatalf("command sequence:\ngot:  %v\nwant: %v", fake.trace, expected)
	}
	for index := range expected {
		if fake.trace[index] != expected[index] {
			t.Errorf("command %d:\ngot:  %s\nwant: %s", index, fake.trace[index], expected[index])
		}
	}
}
