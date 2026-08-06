package migration

import (
	"context"
	"strconv"
	"strings"
)

// The NBD host surface, shared by the source exports (export-source, export-base)
// and the target clients (clone-target, receive-base) and torn down by cleanup.
// STAGE 1 transport is plain TCP on the source's public IPv4 — unencrypted, a
// deliberate get-it-working-first shortcut (spec/24 §2.1); the seam is kept so a
// future WireGuard/SSH carrier drops in without touching phase logic.

// ensureNBDExport serves a device (a block device OR a plain file, e.g. the
// image-dir tar) read-only over NBD on bindAddress:port, returning the server pid.
// Idempotent: if a qemu-nbd already holds the port, return its recorded pid rather
// than start a second one (which would EADDRINUSE). Ports migration-export-source's
// _ensure_nbd_export.
//
// qemu-nbd is NOT detached through systemd-run: --fork makes it double-fork and the
// parent exits once the socket is ready, so this is an ordinary exec-and-wait via
// Run. It is deliberately not held as a child pid — the pidfile is the handle
// cleanup kills it by. (Only socat, the forward tunnel's carrier, needs systemd-run,
// because it must survive this daemon restarting; qemu-nbd already survives it.)
func ensureNBDExport(ctx context.Context, cmd commands, device, bindAddress string, port int) (int, error) {
	pidFile := nbdPidFile(port)
	serving, err := nbdPortServing(ctx, cmd, port)
	if err != nil {
		return 0, err
	}
	if serving {
		return recordedPid(ctx, cmd, pidFile)
	}
	// --persistent so a transient client disconnect does not tear the export down;
	// --read-only because the source is the source of truth; --cache=none for a
	// consistent read of the snapshot; --fork returns once the socket is ready.
	if _, err := cmd.Run(
		ctx, "sudo qemu-nbd --persistent --read-only --cache=none --bind={} --port={} --pid-file={} --fork {}",
		bindAddress, port, pidFile, device,
	); err != nil {
		return 0, err
	}
	return recordedPid(ctx, cmd, pidFile)
}

// recordedPid reads a qemu-nbd pidfile. A pidfile that is not there yet (a race a
// re-entry can lose) reads as pid 0, the same "unknown but serving" the Python
// returns — cleanup can still kill by the port's pidfile later.
func recordedPid(ctx context.Context, cmd commands, pidFile string) (int, error) {
	if !cmd.OK(ctx, "sudo test -f {}", pidFile) {
		return 0, nil
	}
	output, err := cmd.Run(ctx, "sudo cat {}", pidFile)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return 0, nil
	}
	return pid, nil
}

// nbdPortServing reports whether a qemu-nbd already listens on port. Read-only, so
// no sudo and no sudoers line: listing listening sockets needs no privilege. The
// answer is the OUTPUT, not an exit code (ss exits zero whether or not the port is
// bound), so this is Run + a token scan rather than a probe — an idempotency guard,
// and a wrong "not serving" reaches a qemu-nbd start that EADDRINUSEs loudly.
func nbdPortServing(ctx context.Context, cmd commands, port int) (bool, error) {
	output, err := cmd.RunUnchecked(ctx, "ss -ltn")
	if err != nil {
		return false, err
	}
	return listeningOn(output, port), nil
}

// listeningOn scans `ss -ltn` output for a listener on port. ss prints the local
// endpoint as `<address>:<port>` (`0.0.0.0:10000`, `[::]:10000`), so a token ending
// in `:<port>` is a match — pure, so the scan is unit-testable with no host.
func listeningOn(output string, port int) bool {
	suffix := ":" + strconv.Itoa(port)
	for _, field := range strings.Fields(output) {
		if strings.HasSuffix(field, suffix) {
			return true
		}
	}
	return false
}

// ensureNBDClient attaches /dev/nbd<slot> to a source export and returns the device
// path the dm-clone reads through. Idempotent, health-checked AND size-verified: a
// slot is trusted only when its client is ALIVE and its size matches the source
// export's, else it is dropped and re-dialed. expectedBytes 0 skips the size check.
// Ports clone-target/receive-base's _ensure_nbd_client.
//
// LIVENESS reads /sys/block/nbdN/pid, never `nbd-client -check`: -check reports
// "connected" off the kernel's stale binding even after the client PROCESS has died
// (a real f1→f2 migration hit exactly this — dead pid, -check exit 0, every read 0
// bytes, dm-clone frozen). SIZE catches a prior migration's leftover on the slot,
// whose wrong size makes dm-clone fail deep with "Invalid argument".
func ensureNBDClient(ctx context.Context, cmd commands, host string, port, slot int, expectedBytes int64) (string, error) {
	device := "/dev/nbd" + strconv.Itoa(slot)
	if nbdClientAlive(ctx, cmd, slot) && expectedBytes != 0 {
		size, err := deviceSizeBytes(ctx, cmd, device)
		if err == nil && size == expectedBytes {
			return device, nil
		}
	}
	// Either the client is dead/stale (its zombie kernel owner must be cleared so the
	// reconnect can take the slot) or it is the wrong export (drop and re-dial). Both
	// are the same idempotent -d; harmless on an already-free slot.
	cmd.RunUnchecked(ctx, "sudo nbd-client -d {}", device)
	// The plain positional form reaches qemu-nbd's default (unnamed) export.
	// Verified on a host: the Python's `-N ""` empty-name form both FAILS qemu-nbd's
	// negotiation ("Exiting") AND cannot be matched by a sudoers grant, because sudo
	// will not match an empty argv element — so the empty `-N ""` is dropped, not
	// ported. -persist re-dials on a transient blip rather than dropping the client.
	if _, err := cmd.Run(ctx, "sudo nbd-client {} {} {} -persist", host, port, device); err != nil {
		return "", err
	}
	return device, nil
}

// nbdClientAlive reports whether /dev/nbd<slot> still has a connected client, read
// from the PRESENCE of /sys/block/nbd<slot>/pid — the attribute the kernel keeps
// while a connection is up and removes on disconnect. It does NOT check that the pid
// there names a live PROCESS: nbd-client's default netlink mode hands the socket to
// the kernel and the configuring process exits at once, so a healthy export has no
// process at that pid — a `test -d /proc/<pid>` reported every healthy netlink client
// dead, which had dropCloneIfSourceDead tearing down fully-hydrated clones. A GUARD
// (it gates the reconnect above and the clone-rebuild in dropCloneIfSourceDead), so
// OK is free: a wrong "dead" costs a re-dial or a re-hydration from 0, never
// correctness — the source snapshot is intact and the dest re-copies. The REPORTED
// liveness poll-hydration surfaces is a separate, three-valued check (clone.go).
func nbdClientAlive(ctx context.Context, cmd commands, slot int) bool {
	return cmd.OK(ctx, "test -e /sys/block/nbd{}/pid", slot)
}

// killNBD tears down a qemu-nbd export: by its recorded pid first (when known), then
// by the port's pidfile so a lost pid still cleans up. Best-effort throughout — a
// re-entered cleanup just finishes the rest. Ports cleanup-source's _kill_nbd.
func killNBD(ctx context.Context, cmd commands, pid, port int) {
	if pid != 0 {
		cmd.RunUnchecked(ctx, "sudo kill {}", pid)
	}
	pidFile := nbdPidFile(port)
	if !cmd.OK(ctx, "sudo test -f {}", pidFile) {
		return
	}
	output, _ := cmd.RunUnchecked(ctx, "sudo cat {}", pidFile)
	if filePid := strings.TrimSpace(output); filePid != "" {
		cmd.RunUnchecked(ctx, "sudo kill {}", filePid)
	}
	cmd.RunUnchecked(ctx, "sudo rm -f {}", pidFile)
}
