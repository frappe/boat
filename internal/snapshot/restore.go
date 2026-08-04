package snapshot

// restore.go is the port of vm-restore.py: resume a VM from its pending memory
// snapshot. It is the firecracker unit's ExecStartPost, run after the jailer
// launched Firecracker, and it is the consume half of the snapshot-stop /
// warm-snapshot lifecycle whose capture half lives beside it.
//
// Two cases. No marker is the common cold boot: the launcher passed --config-file
// and Firecracker already booted the guest, so this is a no-op. A marker means a
// complete vmstate+RAM pair is staged, the launcher started Firecracker IDLE
// (/snapshot/load is pre-boot only), and this loads it and resumes — the guest is
// back at its pre-stop instruction in milliseconds instead of a 60-120s cold boot.
//
// The marker is consumed BEFORE the guest runs again: a resumed guest writes to
// its disk and the saved RAM no longer matches it, so the same snapshot must never
// be loaded twice. ANY failure consumes the marker and returns an error — the unit
// fails, Restart=always relaunches, the launcher sees no marker, and the VM
// cold-boots. The fast path degrades to the default path, never to a wedged unit.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/frappe/boat/internal/paths"
)

const (
	// Firecracker creates the API socket within milliseconds of exec; 10s is a
	// generous ceiling before declaring the launch dead.
	socketWaitTimeout  = 10 * time.Second
	socketPollInterval = 50 * time.Millisecond
	// The socket file can exist a beat before Firecracker accepts connections, so
	// a load retries briefly before a genuine rejection is fatal.
	snapshotLoadRetries = 5
	snapshotLoadBackoff = 100 * time.Millisecond
)

// RestoreResult reports which path a restore took — a cold boot (no marker, the
// common case) or a warm restore from a staged memory snapshot.
type RestoreResult struct {
	ColdBoot bool `json:"cold_boot"`
	Restored bool `json:"restored"`
}

// RestoreVM performs the restore for uuid. A cold boot returns (ColdBoot, nil); a
// warm restore returns (Restored, nil); any failure returns a consumed-marker
// error whose caller (the unit) fails so the relaunch cold-boots.
func RestoreVM(ctx context.Context, cmd commands, uuid string) (RestoreResult, error) {
	vm := paths.ForVirtualMachine(uuid)
	marker := vm.MemorySnapshotMarker()
	if !cmd.OK(ctx, "sudo test -e {}", marker) {
		return RestoreResult{ColdBoot: true}, nil // cold boot: --config-file already booted the guest
	}

	// Warm-golden compatibility guard. A memory snapshot is only loadable on a
	// matching CPU / kernel / Firecracker; on mismatch, consume the marker and fail
	// so the relaunch cold-boots the warm disk — slower, always correct.
	if mismatch, err := signatureMismatch(ctx, cmd, vm); err != nil {
		consumeMarker(ctx, cmd, marker)
		return RestoreResult{}, fmt.Errorf("could not validate the memory snapshot (%w); marker consumed, relaunch cold-boots", err)
	} else if mismatch != "" {
		consumeMarker(ctx, cmd, marker)
		return RestoreResult{}, fmt.Errorf("host signature mismatch (%s); marker consumed, relaunch cold-boots", mismatch)
	}

	if err := loadAndStage(ctx, cmd, vm); err != nil {
		consumeMarker(ctx, cmd, marker)
		return RestoreResult{}, fmt.Errorf("memory-snapshot restore failed (%w); marker consumed, next start cold-boots", err)
	}
	consumeMarker(ctx, cmd, marker)
	if err := cmd.FirecrackerAPI(ctx, vm.APISocketDirectory(), vm.APISocketName(), "PATCH", "/vm", `{"state": "Resumed"}`); err != nil {
		return RestoreResult{}, fmt.Errorf("resume after restore: %w", err)
	}
	return RestoreResult{Restored: true}, nil
}

// loadAndStage waits for the socket, loads the snapshot PAUSED (so the marker can
// be consumed strictly before the guest runs), then stages the identity payload
// into MMDS while still paused.
func loadAndStage(ctx context.Context, cmd commands, vm paths.VirtualMachine) error {
	if err := waitForSocket(ctx, cmd, vm.APISocket()); err != nil {
		return err
	}
	if err := loadSnapshot(ctx, cmd, vm); err != nil {
		return err
	}
	return stageMMDS(ctx, cmd, vm)
}

func consumeMarker(ctx context.Context, cmd commands, marker string) {
	cmd.RunUnchecked(ctx, "sudo rm -f {}", marker)
}

// waitForSocket polls for the API socket to appear. The recorder test scripts the
// socket present, so it returns on the first probe with no real sleep; on a host a
// launch that never produces a socket exhausts the ceiling and fails.
func waitForSocket(ctx context.Context, cmd commands, socket string) error {
	attempts := int(socketWaitTimeout/socketPollInterval) + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if cmd.OK(ctx, "sudo test -S {}", socket) {
			return nil
		}
		if attempt < attempts-1 {
			time.Sleep(socketPollInterval)
		}
	}
	return fmt.Errorf("API socket %s did not appear within %s", socket, socketWaitTimeout)
}

// loadSnapshot PUTs /snapshot/load with the jail-relative pair, resume_vm=false.
func loadSnapshot(ctx context.Context, cmd commands, vm paths.VirtualMachine) error {
	body, err := json.Marshal(map[string]any{
		"snapshot_path": paths.MemorySnapshotVMStateInJail,
		"mem_backend": map[string]any{
			"backend_type": "File",
			"backend_path": paths.MemorySnapshotMemoryInJail,
		},
		"resume_vm": false,
	})
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < snapshotLoadRetries; attempt++ {
		lastErr = cmd.FirecrackerAPI(ctx, vm.APISocketDirectory(), vm.APISocketName(), "PUT", "/snapshot/load", string(body))
		if lastErr == nil {
			return nil
		}
		if attempt < snapshotLoadRetries-1 {
			time.Sleep(snapshotLoadBackoff)
		}
	}
	return lastErr
}

// stageMMDS PUTs the staged identity payload (if any) into the metadata service,
// so a warm clone's freshen unit sees it from its first post-resume poll.
func stageMMDS(ctx context.Context, cmd commands, vm paths.VirtualMachine) error {
	metadata := vm.MetadataFile()
	if !cmd.OK(ctx, "sudo test -e {}", metadata) {
		return nil
	}
	payload, err := cmd.Run(ctx, "sudo cat {}", metadata)
	if err != nil {
		return fmt.Errorf("read the staged metadata payload: %w", err)
	}
	return cmd.FirecrackerAPI(ctx, vm.APISocketDirectory(), vm.APISocketName(), "PUT", "/mmds", payload)
}

// signatureMismatch returns a human diff of captured-vs-live host signature, "" on
// a match (or when none was staged — a same-host fast stop/start pair stages none).
// An unreadable signature counts as a mismatch: never load a pair we cannot
// validate.
func signatureMismatch(ctx context.Context, cmd commands, vm paths.VirtualMachine) (string, error) {
	signaturePath := vm.MemorySnapshotSignature()
	if !cmd.OK(ctx, "sudo test -e {}", signaturePath) {
		return "", nil
	}
	raw, err := cmd.Run(ctx, "sudo cat {}", signaturePath)
	if err != nil {
		return "", fmt.Errorf("read the staged signature: %w", err)
	}
	var captured map[string]any
	if err := json.Unmarshal([]byte(raw), &captured); err != nil {
		return fmt.Sprintf("unreadable host signature: %v", err), nil
	}
	live, err := readHostSignature(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("read the live signature: %w", err)
	}
	liveMap, err := signatureAsMap(live)
	if err != nil {
		return "", err
	}
	return diffSignatures(captured, liveMap), nil
}

// signatureAsMap renders the live signature into the same key space the captured
// file carries, so the two are compared field for field regardless of the struct's
// shape — the port of the Python's dict comparison.
func signatureAsMap(signature hostSignature) (map[string]any, error) {
	encoded, err := json.Marshal(signature)
	if err != nil {
		return nil, err
	}
	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		return nil, err
	}
	return asMap, nil
}

// diffSignatures joins the fields that differ between captured and live, sorted so
// the message is stable.
func diffSignatures(captured, live map[string]any) string {
	keys := map[string]struct{}{}
	for key := range captured {
		keys[key] = struct{}{}
	}
	for key := range live {
		keys[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)

	var differences []string
	for _, key := range ordered {
		if fmt.Sprint(captured[key]) != fmt.Sprint(live[key]) {
			differences = append(differences, fmt.Sprintf("%s: captured %v != live %v", key, captured[key], live[key]))
		}
	}
	joined := ""
	for index, difference := range differences {
		if index > 0 {
			joined += "; "
		}
		joined += difference
	}
	return joined
}
