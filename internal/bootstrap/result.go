package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/hostfacts"
	"github.com/frappe/boat/internal/run"
)

// atlasPython is the Atlas venv's interpreter, and the one path in this file that
// is not Boat's own business.
//
// It is read because the result contract says so: bootstrap-server.py reports
// which CPython the host's remaining Python Tasks run under, Atlas's e2e asserts
// the reported value equals `/var/lib/atlas/venv/bin/python --version` on the
// host, and a port that dropped the key would break the check rather than the
// claim. Boat does not create this venv, does not depend on it, and reports an
// empty string when it is absent — and when the last `atlas <verb>` Task is gone,
// this field and this constant go with it.
const atlasPython = "/var/lib/atlas/venv/bin/python"

// bootstrapJSONPath is the on-disk copy of the same facts. It is not redundant:
// the host dashboard reads it (dashboard/backend/server.py) and an operator on
// the box reads it, neither of whom has the Task's stdout. spec/07 lists it as
// part of the host layout.
const bootstrapJSONPath = "/var/lib/atlas/bootstrap.json"

// Result is what a finished bootstrap reports, field for field the Python's
// BootstrapResult dataclass — because Atlas parses this with `cls(**payload)`
// and a key that is missing, extra or misspelled is a TypeError on the
// controller rather than a wrong value.
type Result struct {
	FirecrackerVersion     string
	JailerVersion          string
	KernelVersion          string
	Architecture           string
	PythonVersion          string
	VCPUsTotal             int
	MemoryMegabytesTotal   int
	PoolDiskGigabytesTotal int
}

// Fields renders the result the way the controller reads it: the Python
// dataclass's field names beside their values, in one map that both the
// ATLAS_RESULT= line and the on-disk bootstrap.json are built from. One map,
// because two renderings of the same contract are two chances to spell a key
// differently.
func (result Result) Fields() map[string]any {
	return map[string]any{
		"firecracker_version":       result.FirecrackerVersion,
		"jailer_version":            result.JailerVersion,
		"kernel_version":            result.KernelVersion,
		"architecture":              result.Architecture,
		"python_version":            result.PythonVersion,
		"vcpus_total":               result.VCPUsTotal,
		"memory_megabytes_total":    result.MemoryMegabytesTotal,
		"pool_disk_gigabytes_total": result.PoolDiskGigabytesTotal,
	}
}

// readResult measures the host the bootstrap just finished and records the
// measurement on disk. Read, never inferred: every number below comes back off
// the host rather than from the fact that the steps above exited zero.
func readResult(ctx context.Context, runner *run.Runner, architecture string) (Result, error) {
	facts, err := hostfacts.Read(ctx, runner)
	if err != nil {
		return Result{}, fmt.Errorf("reading the host's facts: %w", err)
	}
	// hostfacts tolerates a host with no readable thin pool, because `GET /export`
	// must describe a broken host rather than refuse to. A bootstrap may not: the
	// pool was created two steps ago, so an absent one here means the creation did
	// not take — and Atlas reads a missing total as "uncatalogued", which its
	// placement treats as UNLIMITED disk.
	if facts.PoolDiskGigabytesTotal == nil {
		return Result{}, fmt.Errorf("the thin pool %s/%s is not readable after bootstrap created it", volumeGroup, poolName)
	}
	result := Result{
		FirecrackerVersion:     facts.FirecrackerVersion,
		JailerVersion:          binaryVersion(ctx, runner, jailerBinary),
		KernelVersion:          facts.KernelVersion,
		Architecture:           architecture,
		PythonVersion:          versionLine(ctx, runner, atlasPython),
		VCPUsTotal:             facts.VCPUsTotal,
		MemoryMegabytesTotal:   facts.MemoryMegabytesTotal,
		PoolDiskGigabytesTotal: *facts.PoolDiskGigabytesTotal,
	}
	return result, writeBootstrapJSON(ctx, runner, result)
}

// writeBootstrapJSON leaves the same facts on the host.
//
// The 0755 on /var/lib/atlas is deliberate and is the Python's, not a slip:
// makeDirectories creates the tree 0700 so no other user can enumerate a guest's
// disks, and this one directory is then opened up so that the readers of the file
// below — the dashboard's unprivileged backend, and the non-root `boat` user the
// units run as — can traverse to it. The subdirectories stay 0700; only the
// listing of this level and this 0644 file become readable.
func writeBootstrapJSON(ctx context.Context, runner *run.Runner, result Result) error {
	encoded, err := json.Marshal(result.Fields())
	if err != nil {
		return err
	}
	if err := runner.InstallDirectory(ctx, "/var/lib/atlas", "0755"); err != nil {
		return err
	}
	return runner.InstallFile(ctx, string(encoded), bootstrapJSONPath, "0644")
}

// versionLine is `<binary> --version`, first line, trimmed — and the empty string
// when the binary is not there.
//
// Absence is not a failure, exactly as the Python's `|| true`: the jailer is read
// right after it was installed, so an empty value is worth reporting rather than
// aborting over, and the Atlas venv legitimately does not exist on a host Boat
// bootstrapped by itself.
func versionLine(ctx context.Context, runner *run.Runner, binary string) string {
	output, err := runner.RunUnchecked(ctx, "{} --version", binary)
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimSpace(output), "\n")
	return first
}

// binaryVersion is the version TOKEN off that line — `awk '{print $2}'`, the
// Python's `_binary_version`.
//
// The two readings are not interchangeable and the Python distinguishes them for
// a reason Atlas can see: `firecracker_version` is compared to the version that
// was asked for ("v1.16.0"), while `python_version` is displayed whole ("Python
// 3.14.3"). Reporting either in the other's shape breaks a comparison or a log
// line at the far end.
func binaryVersion(ctx context.Context, runner *run.Runner, binary string) string {
	return secondField(versionLine(ctx, runner, binary))
}

func secondField(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}
