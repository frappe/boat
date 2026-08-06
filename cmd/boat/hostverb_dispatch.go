package main

import "io"

// A host verb is a host operation Atlas used to drive as `boat <verb>` over an
// SSH connection: provision-vm, the snapshot family, sync-image, per-VM
// networking. This file is the seam that lets the daemon run those SAME verb
// functions in-process behind POST /host-verbs/{verb}, so a verb has one
// implementation whether the operator types `boat snapshot-vm` or Atlas posts
// the operation — the CLI stops being a second path into the host and becomes a
// client of the daemon like every other verb (spec/33 §2.1, §2.4).
//
// hostVerbRunner is the cmd/boat half of api.HostVerbRunner. It is injected into
// api.Dependencies (parts.go) rather than importing cmd/boat into internal/api —
// the verb functions live here beside the flag tables they answer, and the
// daemon reaches them through this narrow interface.

// servedHostVerbs is the closed set the daemon runs over the endpoint. It is a
// SUBSET of the verbs Run dispatches, so a verb is written and testable before it
// is switched on — and it is switched on only once the boat user holds every grant
// the verb needs (internal/allowlist proves that), which is why these six lead.
//
// These six reach ZERO privileged command the allow-list does not already grant:
// each shares its host mechanics with a verb the daemon already runs (a disk
// snapshot is the lifecycle LVM path, the memory snapshot is sleep-vm's, host keys
// and firewall and base-ship cleanup are migration's), so serving them adds no new
// standing privilege to the boat user (§12).
//
// NOT served yet, and why:
//   - provision-vm, sync-image, promote-snapshot-image, warm-snapshot-vm, the two
//     s3 backups: each needs grants the boat user does not hold (curl, mkfs.ext4,
//     dd, `systemctl start` an arbitrary unit), which must be written scoped and
//     proven on a host with `sudo -u boat -n -l` first — see hostVerbEntryPoints in
//     internal/allowlist. vm-tunnel additionally renders `sudo {}` (its wireguard
//     commands are computed), which no grant can safely authorise until they are
//     literalised. Their transport is READY; enabling one is adding it here and to
//     hostVerbEntryPoints together with its grants.
//   - bootstrap and reset-server bookend the daemon's own existence — one brings
//     the host up before there is a daemon to answer HTTP, the other tears the host
//     down and stops boat.service — so neither can be driven THROUGH the daemon.
//   - poll-vm-traffic and probe-woken-vms are read-only per-minute sweeps Atlas
//     runs through run_probe with no Task row; a journaled POST would write ~2,880
//     operation records per host per day. They want a non-journaling read path.
var servedHostVerbs = map[string]bool{
	"snapshot-vm":             true,
	"snapshot-stop-vm":        true,
	"delete-snapshot-vm":      true,
	"regenerate-host-keys-vm": true,
	"firewall-apply":          true,
	"export-cleanup-source":   true,
}

type hostVerbRunner struct{}

// Serves reports whether the daemon runs the named verb, so the boundary refuses
// an unknown one with 400 before it claims an operation identifier.
func (hostVerbRunner) Serves(verb string) bool { return servedHostVerbs[verb] }

// Run executes the verb in-process, exactly as `boat <verb>` runs it over SSH,
// writing its trace to stderr and its one ATLAS_RESULT= line (where the verb has
// a result) to stdout. It returns the exit code the equivalent Task carried.
//
// These are the same functions main.go's dispatch calls, so the daemon and the
// CLI can never run two different implementations of one verb. A verb Run does
// not know returns exitUsage, but the boundary has already refused it by Serves,
// so that branch is unreachable in production.
func (hostVerbRunner) Run(verb string, arguments []string, stdout, stderr io.Writer) int {
	switch verb {
	case "provision-vm":
		return provisionVM(arguments, stdout, stderr)
	case "snapshot-vm":
		return snapshotVM(arguments, stdout, stderr)
	case "snapshot-stop-vm":
		return snapshotStopVM(arguments, stdout, stderr)
	case "warm-snapshot-vm":
		return warmSnapshotVM(arguments, stdout, stderr)
	case "delete-snapshot-vm":
		return deleteSnapshotVM(arguments, stderr)
	case "upload-snapshot-s3":
		return uploadSnapshotS3(arguments, stdout, stderr)
	case "restore-snapshot-s3":
		return restoreSnapshotS3(arguments, stdout, stderr)
	case "sync-image":
		return syncImage(arguments, stderr)
	case "promote-snapshot-image":
		return promoteSnapshotImage(arguments, stdout, stderr)
	case "regenerate-host-keys-vm":
		return regenerateHostKeysVM(arguments, stderr)
	case "firewall-apply":
		return firewallApply(arguments, stderr)
	case "vm-tunnel":
		return vmTunnel(arguments, stdout, stderr)
	case "export-cleanup-source":
		return exportCleanupSource(arguments, stderr)
	}
	return exitUsage
}
