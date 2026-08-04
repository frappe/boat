package update

// staging.go is the on-disk handoff between the two halves of a self-update that
// run in different processes. The daemon's POST /v1/update handler verifies a
// pushed release and writes it here, under its StateDirectory; the DETACHED
// `boat update-apply` process — spawned outside boat.service's cgroup so it
// survives the restart — reads it back and runs Apply against it.
//
// The layout lives in this one file, beside the Release it serialises, so the
// writer (internal/api) and the reader (cmd/boat) can never drift on where the
// bytes are or how the signature is framed. It is deliberately tiny: a regular
// file for the binary and a small JSON manifest for the version, checksum and
// detached signature — everything update-apply needs to reconstruct a Release
// and RE-VERIFY it before it swaps anything (defence in depth: the daemon
// verified once already, and the updater verifies again on the binary it is
// about to rename over /usr/local/bin/boat).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// stagedBinaryName is the release binary as install.go will stage-and-swap it.
	// 0755 because it is an executable the updater hands to `sudo install`.
	stagedBinaryName = "boat"
	// stagedManifestName carries the version, checksum and signature — the three
	// things Verify needs that the bare binary does not carry in itself.
	stagedManifestName = "manifest.json"
)

// stagedManifest is the JSON beside the binary. The signature is []byte, so
// encoding/json renders it base64 — the same framing the wire UpdateRequest
// uses, kept identical on purpose so a byte that survived the HTTP hop survives
// the disk hop too.
type stagedManifest struct {
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	Signature []byte `json:"signature"`
}

// WriteStaged persists a verified release into dir, creating it if need be. The
// caller (the daemon) owns dir under its StateDirectory — systemd creates
// /var/lib/boat 0700 owned by the boat user — so nothing unprivileged can read
// the staged binary before it is swapped, and no sudo is needed to write it.
//
// The binary is written FIRST and the manifest LAST, so a reader that finds a
// manifest is guaranteed to find the binary it describes: ReadStaged keys off the
// binary and the manifest together, and the order here is what makes a
// half-written stage read as absent rather than as a release with no bytes.
func WriteStaged(dir string, release Release) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the update staging directory %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, stagedBinaryName), release.Binary, 0o755); err != nil {
		return fmt.Errorf("stage the release binary: %w", err)
	}
	manifest, err := json.Marshal(stagedManifest{
		Version:   release.Manifest.Version,
		SHA256:    release.Manifest.SHA256,
		Signature: release.Signature,
	})
	if err != nil {
		return fmt.Errorf("encode the staged manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, stagedManifestName), manifest, 0o600); err != nil {
		return fmt.Errorf("stage the release manifest: %w", err)
	}
	return nil
}

// ReadStaged reconstructs the Release the daemon staged into dir. The updater
// calls it and then re-runs Verify: the bytes read here are the bytes about to
// be renamed over the live binary, so they are re-checked against the same signed
// manifest rather than trusted because the daemon said so once.
func ReadStaged(dir string) (Release, error) {
	binary, err := os.ReadFile(filepath.Join(dir, stagedBinaryName))
	if err != nil {
		return Release{}, fmt.Errorf("read the staged release binary: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, stagedManifestName))
	if err != nil {
		return Release{}, fmt.Errorf("read the staged release manifest: %w", err)
	}
	var manifest stagedManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Release{}, fmt.Errorf("decode the staged release manifest: %w", err)
	}
	return Release{
		Manifest:  Manifest{Version: manifest.Version, SHA256: manifest.SHA256},
		Binary:    binary,
		Signature: manifest.Signature,
	}, nil
}
