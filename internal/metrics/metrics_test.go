package metrics

import (
	"strings"
	"testing"
)

func TestRenderIsPrometheusText(t *testing.T) {
	rendered := Metrics{
		CPUCores: 8, MemoryBytes: 16777216000, DiskFreeBytes: 300000000000,
		PoolDataPercent: 12.5, PoolMetadataPercent: 3, VirtualMachines: 2, FirecrackerRunning: 1,
	}.Render()

	for _, want := range []string{
		"# TYPE boat_host_cpu_cores gauge\nboat_host_cpu_cores 8\n",
		"boat_pool_data_percent 12.5\n",
		"boat_virtual_machines_total 2\n",
		"boat_firecracker_running_total 1\n",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered metrics missing %q:\n%s", want, rendered)
		}
	}
}

// An unmeasured value renders as NaN, not a misleading 0 — a host with no pool yet
// reports unknown fullness, not empty.
func TestUnmeasuredValueRendersNaN(t *testing.T) {
	rendered := Metrics{CPUCores: 8, MemoryBytes: -1, DiskFreeBytes: -1, PoolDataPercent: -1, PoolMetadataPercent: -1}.Render()
	if !strings.Contains(rendered, "boat_pool_data_percent NaN\n") {
		t.Errorf("an unmeasured pool percent did not render NaN:\n%s", rendered)
	}
	if strings.Contains(rendered, "boat_host_memory_bytes -1") {
		t.Error("an unmeasured value rendered as a negative number")
	}
}
