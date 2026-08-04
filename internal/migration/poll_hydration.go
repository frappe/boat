package migration

import "context"

// PollHydrationParams optionally overrides which dm-clone is polled. Empty (the
// usual case) polls a migrating VM's disk clones — root plus the data clone when it
// exists — and reports the MIN, so the phase only advances when BOTH disks are fully
// hydrated. Set to a single dm device (atlas-base-<image>-clone) it polls that one
// instead, which is how the local-base-image ship reuses this percent.
type PollHydrationParams struct {
	CloneDevice string
}

// PollHydrationResult is the per-tick reading the controller advances on: the
// percent, and whether the source is still alive. SourceHealthy false means the
// source nbd client died and the controller must re-run prepare (which rebuilds the
// stack); the percent is meaningless on a dead source and is not advanced on.
type PollHydrationResult struct {
	HydrationPercent int
	SourceHealthy    bool
}

// PollHydration is the Hydrating phase, called once per controller tick: it enables
// hydration on first touch (idempotent) and reports the current percent across the
// VM's disk clone(s). It is a poll that also MUTATES (enable_hydration) — the copy
// is kept off the worker as a cheap probe the controller drives.
//
// A missing clone reads as 100%: it was either never created or already collapsed
// (cutover ran), so a re-entry after collapse advances cleanly. A dead source is
// reported unhealthy rather than as a percent. Ports scripts/migration-poll-hydration.py.
func PollHydration(ctx context.Context, cmd commands, uuid string, params PollHydrationParams) (PollHydrationResult, error) {
	names, err := pollTargets(ctx, cmd, uuid, params)
	if err != nil {
		return PollHydrationResult{}, err
	}

	var percents []int
	healthy := true
	for _, name := range names {
		// A clone that is gone counts as fully hydrated — collapse already ran, or it
		// was never built. dmsetup info is a guard here (OK): a host with no such
		// device is the ordinary "gone" case, and the mutations below fail loudly on a
		// device that truly is not there.
		if !cmd.OK(ctx, "sudo dmsetup info {}", name) {
			percents = append(percents, 100)
			continue
		}
		// The one three-valued check: a dead source freezes hydration while dmsetup
		// still reports the clone present, so its liveness is REPORTED (drives a
		// rebuild), and an Unknown must surface as an error rather than round to
		// "dead" and trigger a destructive re-prepare.
		alive, err := cloneSourceAlive(ctx, cmd, name)
		if err != nil {
			return PollHydrationResult{}, err
		}
		if !alive {
			healthy = false
			continue // percent is meaningless on a dead source; skip enable + read
		}
		if _, err := cmd.Run(ctx, "sudo dmsetup message {} 0 enable_hydration", name); err != nil {
			return PollHydrationResult{}, err
		}
		status, err := cmd.Run(ctx, "sudo dmsetup status {}", name)
		if err != nil {
			return PollHydrationResult{}, err
		}
		percent, err := hydrationPercent(status)
		if err != nil {
			return PollHydrationResult{}, err
		}
		percents = append(percents, percent)
	}

	return PollHydrationResult{HydrationPercent: reportPercent(percents, healthy), SourceHealthy: healthy}, nil
}

// pollTargets is the set of dm devices to poll: the explicit override, or the VM's
// root clone plus its data clone when one exists.
func pollTargets(ctx context.Context, cmd commands, uuid string, params PollHydrationParams) ([]string, error) {
	if params.CloneDevice != "" {
		return []string{params.CloneDevice}, nil
	}
	if _, err := NBDPort(uuid); err != nil {
		return nil, err // reject a malformed UUID before it names a device
	}
	names := []string{vmCloneName(uuid)}
	if cmd.OK(ctx, "sudo dmsetup info {}", vmCloneName(uuid+"-data")) {
		names = append(names, vmCloneName(uuid+"-data"))
	}
	return names, nil
}

// reportPercent is the MIN across the polled clones — the phase advances only when
// every disk is done. With no live clone left it is 100 when all were gone/collapsed
// and 0 when the only reason there is no percent is a dead source.
func reportPercent(percents []int, healthy bool) int {
	if len(percents) == 0 {
		if healthy {
			return 100
		}
		return 0
	}
	minimum := percents[0]
	for _, percent := range percents[1:] {
		if percent < minimum {
			minimum = percent
		}
	}
	return minimum
}
