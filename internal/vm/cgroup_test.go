package vm

import (
	"slices"
	"testing"

	"github.com/frappe/boat/internal/model"
)

// The values pinned here are the ones atlas/tests/test_networking.py asserts of
// networking.cgroup_args. They are a conformance gate, not a description of this
// function: the launcher a resize rewrites is the launcher provision generated
// from the Python's output, so a divergence caps a guest below the RAM it was
// granted and the kernel OOM-kills Firecracker on the next boot.
func TestCgroupArgumentsRenderWhatAtlasRenders(t *testing.T) {
	for name, testCase := range map[string]struct {
		desired model.DesiredVirtualMachine
		want    []string
	}{
		// test_cgroup_args_for_resource_triple: two whole cores' bandwidth.
		"whole cores, hard cap": {
			desired: model.DesiredVirtualMachine{
				VCPUs: 2, CPUMaxCores: 2, MemoryMegabytes: 1024, DiskGigabytes: 8, CPUMode: "Hard cap",
			},
			want: []string{"memory.max=1342177280", "memory.swap.max=0", "cpu.max=200000 100000"},
		},
		// test_cgroup_args_fractional_cpu_cap: a 1/16 tier is 6250 microseconds of
		// every 100000, and the guest still boots one vCPU thread.
		"one sixteenth of a core": {
			desired: model.DesiredVirtualMachine{
				VCPUs: 1, CPUMaxCores: 0.0625, MemoryMegabytes: 256, DiskGigabytes: 4, CPUMode: "Hard cap",
			},
			want: []string{"memory.max=536870912", "memory.swap.max=0", "cpu.max=6250 100000"},
		},
		"one eighth of a core": {
			desired: model.DesiredVirtualMachine{
				VCPUs: 1, CPUMaxCores: 0.125, MemoryMegabytes: 512, DiskGigabytes: 6, CPUMode: "Hard cap",
			},
			want: []string{"memory.max=805306368", "memory.swap.max=0", "cpu.max=12500 100000"},
		},
		// test_cgroup_args_relaxed_mode_weight_and_burst_ceiling: the weight is the
		// guaranteed share, the ceiling is one whole core to burst into.
		"one sixteenth, relaxed": {
			desired: model.DesiredVirtualMachine{
				VCPUs: 1, CPUMaxCores: 0.0625, MemoryMegabytes: 256, DiskGigabytes: 4, CPUMode: "Relaxed",
			},
			want: []string{"memory.max=536870912", "memory.swap.max=0", "cpu.weight=6", "cpu.max=100000 100000"},
		},
		// test_cgroup_args_relaxed_ceiling_tracks_vcpus.
		"four cores, relaxed": {
			desired: model.DesiredVirtualMachine{
				VCPUs: 4, CPUMaxCores: 4, MemoryMegabytes: 1024, DiskGigabytes: 8, CPUMode: "Relaxed",
			},
			want: []string{"memory.max=1342177280", "memory.swap.max=0", "cpu.weight=400", "cpu.max=400000 100000"},
		},
		// A record stating no mode takes the hard cap, which is the branch the
		// Python's if/else falls through to.
		"no stated mode is the hard cap": {
			desired: model.DesiredVirtualMachine{
				VCPUs: 1, CPUMaxCores: 1, MemoryMegabytes: 512, DiskGigabytes: 10,
			},
			want: []string{"memory.max=805306368", "memory.swap.max=0", "cpu.max=100000 100000"},
		},
	} {
		if got := CgroupArguments(testCase.desired); !slices.Equal(got, testCase.want) {
			t.Errorf("%s:\ngot:  %q\nwant: %q", name, got, testCase.want)
		}
	}
}

// test_cpu_weight_scales_and_clamps, verbatim. The clamps are what keep the
// smallest tier from rounding to a weight of zero, which the kernel rejects.
func TestCpuWeightScalesAndClamps(t *testing.T) {
	for _, testCase := range []struct {
		cores float32
		want  int
	}{
		{1, 100},
		{0.0625, 6},
		{2, 200},
		{0.001, cpuWeightMinimum},
		{1000, cpuWeightMaximum},
	} {
		if got := cpuWeight(testCase.cores); got != testCase.want {
			t.Errorf("cpuWeight(%v) = %d, want %d", testCase.cores, got, testCase.want)
		}
	}
}

// Python's round() breaks a tie toward the even number and Go's math.Round
// breaks it away from zero. cpu_max_cores of 0.125 puts the weight exactly on a
// tie, so the two would disagree on a size Atlas actually sells.
func TestATieRoundsTheWayPythonRoundsIt(t *testing.T) {
	if got := roundHalfToEven(12.5); got != 12 {
		t.Errorf("roundHalfToEven(12.5) = %d, want 12 (Python's round)", got)
	}
	if got := roundHalfToEven(13.5); got != 14 {
		t.Errorf("roundHalfToEven(13.5) = %d, want 14 (Python's round)", got)
	}
}
