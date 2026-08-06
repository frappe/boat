// Package metricspush turns a host's gathered metrics and its VM list into datum
// samples, grouped by the resource that owns them: the host's own gauges, and each
// VM's cgroup and network counters keyed by UUID. Reads are from injectable
// filesystem roots so the whole mapping is unit-testable without a real host, and
// every per-VM read is best-effort: a missing cgroup or veth file (a VM that is not
// running, or was torn down between ticks) drops that one sample, never the batch.
package metricspush

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/datum"
	"github.com/frappe/boat/internal/metrics"
	"github.com/frappe/boat/internal/model"
)

// Roots are the filesystem trees the collector reads. Production uses DefaultRoots;
// tests point them at a t.TempDir().
type Roots struct {
	Cgroup      string // /sys/fs/cgroup
	Proc        string // /proc
	SysClassNet string // /sys/class/net
}

// DefaultRoots are the real host paths.
func DefaultRoots() Roots {
	return Roots{Cgroup: "/sys/fs/cgroup", Proc: "/proc", SysClassNet: "/sys/class/net"}
}

// Grouped is samples ready to push, split by the resource_id that owns them: Host
// under the host token, and each VM's slice under that VM's token (keyed by UUID).
type Grouped struct {
	Host []datum.Sample
	VMs  map[string][]datum.Sample
}

// Collect builds all samples for one tick at timestamp ts (RFC3339 with Z). host is
// the already-gathered host metrics; vms is the store's VM list.
func Collect(ts, serverName string, host metrics.Metrics, vms []model.VirtualMachine, roots Roots) Grouped {
	grouped := Grouped{Host: hostSamples(ts, host, len(vms)), VMs: map[string][]datum.Sample{}}
	for _, vm := range vms {
		grouped.VMs[vm.UUID] = vmSamples(ts, serverName, vm, roots)
	}
	return grouped
}

func hostSamples(ts string, m metrics.Metrics, virtualMachineCount int) []datum.Sample {
	var out []datum.Sample
	add := func(metric string, value float64) {
		out = append(out, datum.Sample{Metric: metric, Value: value, TS: ts})
	}
	add("host_up", 1)
	add("host_cpu_cores", float64(m.CPUCores))
	if m.MemoryBytes >= 0 {
		add("host_memory_bytes", float64(m.MemoryBytes))
	}
	if m.DiskFreeBytes >= 0 {
		add("host_disk_free_bytes", float64(m.DiskFreeBytes))
	}
	if m.PoolDataPercent >= 0 {
		add("host_pool_data_percent", m.PoolDataPercent)
	}
	if m.PoolMetadataPercent >= 0 {
		add("host_pool_metadata_percent", m.PoolMetadataPercent)
	}
	// The store's VM list is what boat actually holds; metrics.Gather's own count
	// reads a root-only directory without sudo and would report 0 on a real host.
	add("host_virtual_machines", float64(virtualMachineCount))
	add("host_firecracker_running", float64(m.FirecrackerRunning))
	return out
}

func vmSamples(ts, serverName string, vm model.VirtualMachine, roots Roots) []datum.Sample {
	labels := map[string]string{"server": serverName}
	upLabels := map[string]string{"server": serverName, "status": string(vm.ObservedStatus)}

	out := []datum.Sample{{Metric: "vm_up", Value: boolToFloat(vm.ObservedStatus == model.StatusRunning), TS: ts, Labels: upLabels}}
	if vm.ObservedStatus != model.StatusRunning {
		return out
	}

	add := func(metric string, value float64) {
		out = append(out, datum.Sample{Metric: metric, Value: value, TS: ts, Labels: labels})
	}

	if dir := cgroupDir(vm, roots); dir != "" {
		if v, ok := readUint(filepath.Join(dir, "memory.current")); ok {
			add("vm_memory_current_bytes", v)
		}
		if v, ok := readMemoryMax(dir); ok {
			add("vm_memory_max_bytes", v)
		}
		if v, ok := readCPUUsageSeconds(dir); ok {
			add("vm_cpu_usage_seconds_total", v)
		}
		if read, written, ok := readIO(dir); ok {
			add("vm_io_read_bytes_total", read)
			add("vm_io_write_bytes_total", written)
		}
	}

	veth := hostVeth(vm.UUID)
	if veth != "" {
		if rx, ok := readUint(filepath.Join(roots.SysClassNet, veth, "statistics", "rx_bytes")); ok {
			add("vm_network_transmit_bytes_total", rx) // host rx = guest egress
		}
		if tx, ok := readUint(filepath.Join(roots.SysClassNet, veth, "statistics", "tx_bytes")); ok {
			add("vm_network_receive_bytes_total", tx) // host tx = guest ingress
		}
	}
	return out
}

// cgroupDir resolves a running VM's cgroup v2 directory: the default
// <cgroup>/firecracker/<uuid>, else the path named in <proc>/<pid>/cgroup. Returns
// "" if neither exists (the VM is not running).
func cgroupDir(vm model.VirtualMachine, roots Roots) string {
	def := filepath.Join(roots.Cgroup, "firecracker", vm.UUID)
	if isDir(def) {
		return def
	}
	if vm.FirecrackerPID > 0 {
		if path := parseProcCgroup(filepath.Join(roots.Proc, strconv.Itoa(vm.FirecrackerPID), "cgroup")); path != "" {
			candidate := filepath.Join(roots.Cgroup, path)
			if isDir(candidate) {
				return candidate
			}
		}
	}
	return ""
}

// parseProcCgroup returns the cgroup-v2 path from a /proc/<pid>/cgroup file, i.e.
// the part after "0::" on the unified line, or "" if not found.
func parseProcCgroup(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func hostVeth(uuid string) string {
	hex := strings.ReplaceAll(strings.ToLower(uuid), "-", "")
	if len(hex) < 7 {
		return ""
	}
	return "atlas-h" + hex[:7]
}

func readUint(path string) (float64, bool) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(content)), 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(value), true
}

func readMemoryMax(dir string) (float64, bool) {
	content, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		return 0, false
	}
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "max" {
		return 0, false
	}
	value, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(value), true
}

func readCPUUsageSeconds(dir string) (float64, bool) {
	content, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			micros, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, false
			}
			return float64(micros) / 1e6, true
		}
	}
	return 0, false
}

func readIO(dir string) (read, written float64, ok bool) {
	content, err := os.ReadFile(filepath.Join(dir, "io.stat"))
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(content), "\n") {
		for _, field := range strings.Fields(line) {
			if suffix, found := strings.CutPrefix(field, "rbytes="); found {
				if v, err := strconv.ParseUint(suffix, 10, 64); err == nil {
					read += float64(v)
				}
			}
			if suffix, found := strings.CutPrefix(field, "wbytes="); found {
				if v, err := strconv.ParseUint(suffix, 10, 64); err == nil {
					written += float64(v)
				}
			}
		}
	}
	return read, written, true
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
