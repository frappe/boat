// Package metrics gathers what a host operator watches — CPU and memory capacity,
// thin-pool fullness, and how many VMs the host holds and runs — and renders it in
// Prometheus text format. `boat metrics` prints it; pointed at a node_exporter
// textfile collector (or scraped from a small server) it becomes the host's metric
// feed, with no agent the fleet doesn't already run.
//
// Every value is measured off the host through the same run.Runner every verb uses
// — the pool percentages come from lvs, the VM counts from the tree and the process
// table — so the numbers are the host's own, not a controller's stale mirror.
package metrics

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/run"
)

// Metrics is one host's gauges. A field is -1 when it could not be measured, so a
// rendered `NaN` says "unknown" rather than a misleading zero.
type Metrics struct {
	CPUCores            int
	MemoryBytes         int64
	DiskFreeBytes       int64
	PoolDataPercent     float64
	PoolMetadataPercent float64
	VirtualMachines     int
	FirecrackerRunning  int
}

const (
	poolReference     = "atlas/pool0"
	virtualMachineDir = "/var/lib/atlas/virtual-machines"
)

// Gather measures the host. Every probe is best-effort: a host with no pool yet
// (pre-bootstrap) still reports its CPU and memory rather than failing.
func Gather(ctx context.Context, runner *run.Runner) Metrics {
	metrics := Metrics{
		CPUCores:            atoi(runField(ctx, runner, "nproc"), 0),
		MemoryBytes:         atoi64(runField(ctx, runner, "awk '/MemTotal/{print $2*1024}' /proc/meminfo"), -1),
		DiskFreeBytes:       atoi64(diskFree(ctx, runner), -1),
		PoolDataPercent:     atof(runField(ctx, runner, "sudo lvs --noheadings --nosuffix -o data_percent {}", poolReference), -1),
		PoolMetadataPercent: atof(runField(ctx, runner, "sudo lvs --noheadings --nosuffix -o metadata_percent {}", poolReference), -1),
		VirtualMachines:     atoi(runField(ctx, runner, "sh -c {}", "ls -1 "+virtualMachineDir+" 2>/dev/null | wc -l"), 0),
		FirecrackerRunning:  atoi(runField(ctx, runner, "sh -c {}", "pgrep -c firecracker || true"), 0),
	}
	return metrics
}

// Render is the Prometheus exposition text — one HELP/TYPE/value trio per gauge.
func (metrics Metrics) Render() string {
	var builder strings.Builder
	gauge := func(name, help string, value float64) {
		fmt.Fprintf(&builder, "# HELP %s %s\n# TYPE %s gauge\n%s %s\n", name, help, name, name, formatValue(value))
	}
	gauge("boat_host_cpu_cores", "Usable CPU cores on the host.", float64(metrics.CPUCores))
	gauge("boat_host_memory_bytes", "Total host RAM in bytes.", knownOrNaN(metrics.MemoryBytes))
	gauge("boat_host_disk_free_bytes", "Free bytes on the atlas filesystem.", knownOrNaN(metrics.DiskFreeBytes))
	gauge("boat_pool_data_percent", "Thin-pool data usage percent.", knownFloat(metrics.PoolDataPercent))
	gauge("boat_pool_metadata_percent", "Thin-pool metadata usage percent.", knownFloat(metrics.PoolMetadataPercent))
	gauge("boat_virtual_machines_total", "VM directories the host holds.", float64(metrics.VirtualMachines))
	gauge("boat_firecracker_running_total", "Firecracker processes currently running.", float64(metrics.FirecrackerRunning))
	return builder.String()
}

func diskFree(ctx context.Context, runner *run.Runner) string {
	// The atlas tree, falling back to root before bootstrap makes the directory.
	output := runField(ctx, runner, "sh -c {}", "df -B1 --output=avail /var/lib/atlas 2>/dev/null | tail -1 || df -B1 --output=avail / | tail -1")
	return strings.TrimSpace(output)
}

func runField(ctx context.Context, runner *run.Runner, template string, parameters ...any) string {
	output, err := runner.RunUnchecked(ctx, template, parameters...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(output)
}

func atoi(value string, fallback int) int {
	if parsed, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
		return parsed
	}
	return fallback
}

func atoi64(value string, fallback int64) int64 {
	if parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
		return parsed
	}
	return fallback
}

func atof(value string, fallback float64) float64 {
	if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
		return parsed
	}
	return fallback
}

func knownOrNaN(value int64) float64 {
	if value < 0 {
		return nan()
	}
	return float64(value)
}

func knownFloat(value float64) float64 {
	if value < 0 {
		return nan()
	}
	return value
}

func nan() float64 { return sentinelNaN }

// sentinelNaN is rendered as Prometheus "NaN".
var sentinelNaN = func() float64 { var zero float64; return zero / zero }()

func formatValue(value float64) string {
	if value != value { // NaN
		return "NaN"
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}
