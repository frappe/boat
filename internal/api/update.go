package api

// update.go is POST /v1/update: the entry point of a self-update (§5). It does the
// smallest safe thing and hands the rest off — verify the pushed release, stage
// it, and launch a DETACHED updater outside this daemon's cgroup — because the one
// thing it must not do is run the update itself.
//
// # Why the daemon only STARTS the update
//
// The update's step 4 is `systemctl restart boat.service`, which SIGTERMs the
// whole boat.service cgroup. An updater running inside the daemon would be killed
// by the very restart it issued, mid-swap, with no process left to health-check
// the new binary or roll back to N-1 — the host would be left on whichever binary
// the SIGTERM happened to interrupt. So the daemon spawns `boat update-apply` into
// a transient systemd SCOPE, which lives in its own cgroup under system.slice and
// survives the restart, and answers 202 at once. The update proceeds there; Atlas
// sees the result as the running version changing on /export.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/frappe/boat/internal/update"
	"github.com/frappe/boat/internal/wire"
)

const (
	// updaterBinaryPath is the binary the scope runs — the same live binary this
	// daemon is (the busybox model), under the update-apply subcommand. It is the
	// path install.go swaps, quoted literally in the sudoers systemd-run line.
	updaterBinaryPath = "/usr/local/bin/boat"
	// updaterServiceUser is the account the scope runs as, matching boat.service's
	// User=. The updater runs as boat, not root, so its restart/mv/ln commands are
	// gated by the same pinned sudoers allow-list as every other host mutation
	// rather than running with the ambient authority a root scope would carry.
	updaterServiceUser = "boat"
	// updateStagingRoot is the subdirectory of the StateDirectory releases are
	// staged under. <StateDirectory>/update/<id>/ holds the binary and its
	// manifest; the sudoers systemd-run line pins this prefix.
	updateStagingRoot = "update"
	// defaultStateDirectory is boat.service's StateDirectory — where releases stage
	// when Dependencies names no other. systemd creates /var/lib/boat 0700 owned by
	// the boat user, so a staged binary is boat-writable and unprivileged-unreadable.
	defaultStateDirectory = "/var/lib/boat"
)

// Update is POST /v1/update. It verifies the release before anything touches the
// disk, stages the verified bytes, and launches the detached updater — in that
// order, because a release that does not verify must leave no trace and start no
// process (§5: "verify signature and checksum before anything else").
func (server *Server) Update(ctx context.Context, request wire.UpdateRequestObject) (wire.UpdateResponseObject, error) {
	// No trusted key is a policy, not a fault: a host Atlas has not enrolled in
	// self-update accepts none, and says so with a 503 an operator can act on
	// rather than a 400 that would read as "your release is bad".
	if len(server.updateKey) != ed25519.PublicKeySize {
		return &errorResponse{
			statusCode: 503,
			message:    "This host has no trusted update key configured, so it accepts no self-update.",
		}, nil
	}
	if request.Body == nil {
		return &errorResponse{statusCode: 400, message: "This request carried no release to apply."}, nil
	}
	release := releaseFromWire(*request.Body)
	if err := update.Verify(release, server.updateKey); err != nil {
		// The specific reason (bad signature, wrong checksum, malformed manifest)
		// stays on the host in the log; the caller learns only that the release did
		// not verify, which is all it can act on.
		return updateRefused(err), nil
	}

	// Verified. Stage it under the StateDirectory and launch the updater. A staging
	// or spawn failure after a good verify is a host fault (500-class), not a bad
	// request: the release was fine, this host could not act on it.
	id, err := newUpdateIdentifier()
	if err != nil {
		return internalFault("An update identifier could not be generated.", err), nil
	}
	stagingDir := filepath.Join(server.stateDirectory, updateStagingRoot, id)
	if err := update.WriteStaged(stagingDir, release); err != nil {
		return internalFault("The verified release could not be staged.", err), nil
	}
	if err := server.launchUpdater(id, stagingDir); err != nil {
		return internalFault("The detached updater could not be launched.", err), nil
	}
	return wire.Update202JSONResponse{UpdateId: id, Version: release.Manifest.Version}, nil
}

// releaseFromWire rebuilds the update.Release the wire carried. The base64 framing
// of the signature and binary is decoded by the generated model (format: byte), so
// this is a plain field copy — the bytes that verify here are the bytes that will
// be staged and, in the updater, re-verified before the swap.
func releaseFromWire(body wire.UpdateRequest) update.Release {
	return update.Release{
		Manifest:  update.Manifest{Version: body.Version, SHA256: body.Sha256},
		Binary:    body.Binary,
		Signature: body.Signature,
	}
}

// updateRefused maps a verification failure to a 400. Every failure mode is the
// same answer to the caller — the release will not be applied — with the cause
// kept on the host, so a signature attack and a corrupted download read alike to
// an attacker probing the endpoint.
func updateRefused(cause error) *errorResponse {
	message := "This release did not verify and will not be applied."
	switch {
	case errors.Is(cause, update.ErrSignature):
		message = "This release is not signed by this host's trusted update key and will not be applied."
	case errors.Is(cause, update.ErrChecksum):
		message = "This release's bytes do not match its signed checksum and will not be applied."
	case errors.Is(cause, update.ErrManifest):
		message = "This release's manifest is malformed and will not be applied."
	}
	return &errorResponse{statusCode: 400, message: message}
}

// newUpdateIdentifier mints the id an update runs under: 8 random bytes as 16 hex
// characters, the same shape the sudoers systemd-run line and staging path pin. It
// is random rather than a counter so two pushes never collide on a staging
// directory, and fixed-width hex so the sudoers pattern can spell it out.
func newUpdateIdentifier() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read randomness for the update id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// spawnDetachedUpdater is the production launchUpdater: it starts `boat
// update-apply` in a transient systemd scope and returns WITHOUT waiting, so the
// handler answers 202 while the update runs on.
//
// The exact command (pinned byte-for-byte in sudoers.d/boat under
// BOAT_SELF_UPDATE):
//
//		sudo systemd-run --scope --collect --uid=boat --gid=boat \
//		    --unit=boat-update-<id> \
//		    /usr/local/bin/boat update-apply --release /var/lib/boat/update/<id>
//
//	  - --scope puts the updater in its OWN cgroup (a scope under system.slice), so
//	    `systemctl restart boat.service` — which kills boat.service's cgroup — leaves
//	    it running. This is the load-bearing detachment.
//	  - --collect garbage-collects the scope unit even if the updater fails, so a
//	    failed update leaves no lingering `failed` unit behind.
//	  - --uid/--gid drop the updater to the boat service user, so its own
//	    restart/install/mv/ln commands are sudo-gated rather than ambient root.
//
// It does NOT Wait: `systemd-run --scope` stays in the foreground for the WHOLE
// update, so waiting would block the 202. The child is Setsid'd into its own
// session and left to the scope; when this daemon is replaced by the restart, the
// unreaped child is adopted by init. If systemd-run --scope is ever unavailable
// (a host without systemd, or a build of systemd-run that lacks --scope), the
// fallback is a plain Setsid double-fork of `boat update-apply` — which detaches
// from the daemon's session but NOT its cgroup, so it would need boat.service's
// KillMode adjusted to `process` to survive the restart; the scope is preferred
// precisely because it needs no such unit change.
func spawnDetachedUpdater(id, stagingDir string) error {
	command := exec.Command(
		"sudo", "systemd-run", "--scope", "--collect",
		"--uid="+updaterServiceUser, "--gid="+updaterServiceUser,
		"--unit=boat-update-"+id,
		updaterBinaryPath, "update-apply", "--release", stagingDir,
	)
	// A new session detaches the child from the daemon's process group, so a signal
	// aimed at the group does not also hit the updater. The cgroup escape that lets
	// it survive the restart is --scope's job, not this.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start the detached updater: %w", err)
	}
	// Release the child rather than Wait for it: --scope holds for the whole update,
	// and this daemon is about to be restarted out from under it. Its exit is reaped
	// by init once this process is gone.
	return nil
}
