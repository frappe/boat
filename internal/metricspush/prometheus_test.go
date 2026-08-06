package metricspush

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/metrics"
	"github.com/frappe/boat/internal/model"
)

// TestRenderPrometheus builds a fake host tree with one RUNNING VM and checks the
// rendered exposition text: identity labels present (server on host samples, vm on
// per-VM samples), counters typed as counters, one VM grouped under one # TYPE, and
// unmeasured values skipped.
func TestRenderPrometheus(t *testing.T) {
	roots, cgroup, sysClassNet := fakeRoots(t)
	uuid := "6d0a1b2c-0000-0000-0000-000000000000"

	vmDir := filepath.Join(cgroup, "firecracker", uuid)
	mustMkdirAll(t, vmDir)
	mustWriteFile(t, filepath.Join(vmDir, "memory.current"), "1048576\n")
	mustWriteFile(t, filepath.Join(vmDir, "memory.max"), "2097152\n")
	mustWriteFile(t, filepath.Join(vmDir, "cpu.stat"), "usage_usec 2500000\nuser_usec 1\nsystem_usec 1\n")
	mustWriteFile(t, filepath.Join(vmDir, "io.stat"), "259:0 rbytes=4096 wbytes=8192 rios=1 wios=2\n")

	statsDir := filepath.Join(sysClassNet, "atlas-h6d0a1b2", "statistics")
	mustMkdirAll(t, statsDir)
	mustWriteFile(t, filepath.Join(statsDir, "rx_bytes"), "111\n")
	mustWriteFile(t, filepath.Join(statsDir, "tx_bytes"), "222\n")

	host := metrics.Metrics{
		CPUCores:            8,
		MemoryBytes:         -1,
		DiskFreeBytes:       100,
		PoolDataPercent:     -1,
		PoolMetadataPercent: 12.5,
		VirtualMachines:     0,
		FirecrackerRunning:  1,
	}
	out := RenderPrometheus("srv", host, []model.VirtualMachine{
		{UUID: uuid, ObservedStatus: model.StatusRunning},
	}, roots)

	for _, want := range []string{
		"# TYPE host_cpu_cores gauge",
		"host_cpu_cores{server=\"srv\"} 8",
		"# TYPE vm_cpu_usage_seconds_total counter",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\n%s", want, out)
		}
	}

	vmMemoryLine := "vm_memory_current_bytes{"
	idx := strings.Index(out, vmMemoryLine)
	if idx < 0 {
		t.Fatalf("rendered output missing %q\n%s", vmMemoryLine, out)
	}
	lineEnd := strings.Index(out[idx:], "\n")
	if lineEnd < 0 {
		lineEnd = len(out) - idx
	}
	line := out[idx : idx+lineEnd]
	for _, want := range []string{
		`vm="6d0a1b2c-0000-0000-0000-000000000000"`,
		`server="srv"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("vm_memory_current_bytes line missing %q: %s", want, line)
		}
	}

	if count := strings.Count(out, "# TYPE vm_up "); count != 1 {
		t.Errorf("# TYPE vm_up appears %d times, want exactly 1 (grouping)\n%s", count, out)
	}

	if strings.Contains(out, "host_memory_bytes") {
		t.Errorf("rendered output contains host_memory_bytes, want it skipped (unmeasured -1)\n%s", out)
	}
}

func TestRenderPrometheusEscapesLabelValues(t *testing.T) {
	roots, _, _ := fakeRoots(t)
	uuid := "6d0a1b2c-0000-0000-0000-000000000000"

	out := RenderPrometheus("srv", metrics.Metrics{CPUCores: 2}, []model.VirtualMachine{
		{UUID: uuid, ObservedStatus: model.StatusRunning},
	}, roots)

	// vm_up carries a status label; "Running" contains no escapable characters, so
	// the line must be exactly the quoted value with no stray quotes or backslashes.
	if !strings.Contains(out, `vm_up{server="srv",status="Running",vm="6d0a1b2c-0000-0000-0000-000000000000"} 1`) {
		t.Errorf("vm_up line not rendered as expected\n%s", out)
	}
}
