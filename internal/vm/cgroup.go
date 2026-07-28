package vm

import (
	"fmt"
	"math"

	"github.com/frappe/boat/internal/model"
)

// The jailer's cgroup v2 caps, derived from what Atlas desires of a VM.
//
// This is a port of atlas/atlas/networking.py's cgroup_args, and it has to
// render the same strings that function does, byte for byte. provision bakes
// its output into the per-VM jailer-launch.sh and a resize splices fresh values
// into that same launcher (see spliceCgroupArguments), so a Boat that rendered
// them differently would make a re-provision after a resize produce a different
// launcher — and, in the one case that matters, a smaller memory.max than the
// guest RAM the resize just granted, which the kernel resolves by OOM-killing
// Firecracker on the next boot.
const (
	// Headroom over the guest's RAM for the Firecracker process's own VMM, IO and
	// vCPU threads and its page-cache churn, so memory.max bounds the whole
	// process without OOM-killing a healthy VM.
	memoryHeadroomMebibytes = 256

	// The cgroup v2 bandwidth period. cpu.max is written "<quota> <period>", both
	// in microseconds, and the quota is the share of one period the VM may run
	// for — so a quota equal to the period is one whole core's worth.
	cpuPeriodMicroseconds = 100000

	// cpu.weight carries the guaranteed proportional share in relaxed mode.
	// Weights live in [1, 10000] with 100 the default, so scaling by 100 makes a
	// whole core the default weight and a sub-core tier proportionally lighter.
	cpuWeightPerCore = 100
	cpuWeightMinimum = 1
	cpuWeightMaximum = 10000

	// cpuModeRelaxed is Virtual Machine.cpu_mode's relaxed model: a cpu.weight
	// floor under contention plus a loose cpu.max ceiling to burst into idle host
	// CPU. Any other value — "Hard cap", or nothing at all — is the hard cap,
	// which is what the Python's `if cpu_mode == CPU_MODE_RELAXED: … else: …`
	// says and is the safe default for a record that states no mode.
	cpuModeRelaxed = "Relaxed"
)

// CgroupArguments renders the jailer --cgroup values for one VM's desired shape.
//
// Values only, without the repeated --cgroup flag: the flag belongs to whatever
// writes the launcher line, exactly as it does on the Atlas side, where
// _cgroup_values strips the interleaved flag tokens before the Task carries them.
//
// The desired record is taken whole even though the disk is not read, because
// that is the Python's signature too: cgroup_args takes the VM's full resource
// triple and bounds only memory and CPU. A thin LV's growth is bounded by the
// pool's own accounting, not by any per-process limit.
func CgroupArguments(desired model.DesiredVirtualMachine) []string {
	memoryMaxBytes := (desired.MemoryMegabytes + memoryHeadroomMebibytes) * bytesPerMebibyte
	arguments := []string{
		fmt.Sprintf("memory.max=%d", memoryMaxBytes),
		// Never swap guest RAM to host disk — also the per-VM form of
		// Firecracker's own "disable swap / data remanence" guidance.
		"memory.swap.max=0",
	}
	if desired.CPUMode == cpuModeRelaxed {
		// The weight is the floor the VM is guaranteed when the host is busy; the
		// ceiling is vcpus whole cores, which is what it may burst to when it is
		// not. CFS is work-conserving for weights, so both hold at once.
		ceiling := desired.VCPUs * cpuPeriodMicroseconds
		return append(arguments,
			fmt.Sprintf("cpu.weight=%d", cpuWeight(desired.CPUMaxCores)),
			fmt.Sprintf("cpu.max=%d %d", ceiling, cpuPeriodMicroseconds),
		)
	}
	quota := roundHalfToEven(float64(desired.CPUMaxCores) * cpuPeriodMicroseconds)
	return append(arguments, fmt.Sprintf("cpu.max=%d %d", quota, cpuPeriodMicroseconds))
}

// cpuWeight is the weight carrying cpu_max_cores as a proportional share,
// clamped into the kernel's range so the smallest tier never rounds to zero and
// the largest never overflows.
func cpuWeight(cpuMaxCores float32) int {
	return min(cpuWeightMaximum, max(cpuWeightMinimum,
		roundHalfToEven(float64(cpuMaxCores)*cpuWeightPerCore)))
}

// roundHalfToEven is Python's round(), which is what the Atlas side uses and is
// therefore what "byte for byte" means here. Go's math.Round rounds a half away
// from zero, so the two disagree on exactly the values a half lands on — a
// one-eighth-core VM's weight is round(12.5), which is 12 in Python and would be
// 13 under math.Round. Not a hypothetical tier: it is a size Atlas sells.
func roundHalfToEven(value float64) int {
	return int(math.RoundToEven(value))
}
