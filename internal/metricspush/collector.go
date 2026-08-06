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
	"regexp"
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
	// hostSamples(ts, host, len(vms)) would shadow the function name here, so the
	// local is allHost.
	allHost := append(hostSamples(ts, host, len(vms)), hostUtilizationSamples(ts, roots)...)
	grouped := Grouped{Host: allHost, VMs: map[string][]datum.Sample{}}
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

// hostUtilizationSamples reads live host utilization from /proc — all world-readable,
// no sudo: memory used/available, cumulative CPU busy seconds, and host network and
// disk byte counters. Every read is best-effort: a missing or unparseable file drops
// those samples, never the batch.
func hostUtilizationSamples(ts string, roots Roots) []datum.Sample {
	var out []datum.Sample
	add := func(metric string, value float64) {
		out = append(out, datum.Sample{Metric: metric, Value: value, TS: ts})
	}
	if total, available, ok := readMeminfo(roots.Proc); ok {
		add("host_memory_used_bytes", float64(total-available))
		add("host_memory_available_bytes", float64(available))
	}
	if busy, ok := readCPUBusySeconds(roots.Proc); ok {
		add("host_cpu_usage_seconds_total", busy)
	}
	if rx, tx, ok := readNetDev(roots.Proc); ok {
		add("host_network_receive_bytes_total", rx)
		add("host_network_transmit_bytes_total", tx)
	}
	if read, written, ok := readDiskstats(roots.Proc); ok {
		add("host_disk_read_bytes_total", read)
		add("host_disk_write_bytes_total", written)
	}
	return out
}

func readMeminfo(procRoot string) (total, available uint64, ok bool) {
	content, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0, 0, false
	}
	haveTotal, haveAvailable := false, false
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			if value, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				total, haveTotal = value*1024, true
			}
		case "MemAvailable:":
			if value, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				available, haveAvailable = value*1024, true
			}
		}
	}
	return total, available, haveTotal && haveAvailable
}

// readCPUBusySeconds returns cumulative non-idle CPU time in seconds from the
// aggregate "cpu" line of /proc/stat (USER_HZ = 100). It is a counter; rate() gives
// cores used.
func readCPUBusySeconds(procRoot string) (float64, bool) {
	content, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var total, idle uint64
		for index := 1; index < len(fields); index++ {
			value, err := strconv.ParseUint(fields[index], 10, 64)
			if err != nil {
				return 0, false
			}
			total += value
			if index == 4 || index == 5 { // idle + iowait
				idle += value
			}
		}
		return float64(total-idle) / 100.0, true
	}
	return 0, false
}

// readNetDev sums rx/tx bytes over physical host interfaces, excluding loopback and
// the virtual per-VM/bridge devices.
func readNetDev(procRoot string) (receive, transmit float64, ok bool) {
	content, err := os.ReadFile(filepath.Join(procRoot, "net", "dev"))
	if err != nil {
		return 0, 0, false
	}
	excluded := func(name string) bool {
		if name == "lo" {
			return true
		}
		for _, prefix := range []string{"veth", "tap", "atlas-", "docker", "br-", "vnet", "mig6"} {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	any := false
	for _, line := range strings.Split(string(content), "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		if excluded(name) {
			continue
		}
		numbers := strings.Fields(line[colon+1:])
		if len(numbers) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(numbers[0], 10, 64)
		tx, err2 := strconv.ParseUint(numbers[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		receive += float64(rx)
		transmit += float64(tx)
		any = true
	}
	return receive, transmit, any
}

var wholeDiskName = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|nvme[0-9]+n[0-9]+)$`)

// readDiskstats sums read/written bytes across whole block devices (sectors * 512),
// skipping partitions, dm and loop devices.
func readDiskstats(procRoot string) (read, written float64, ok bool) {
	content, err := os.ReadFile(filepath.Join(procRoot, "diskstats"))
	if err != nil {
		return 0, 0, false
	}
	any := false
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !wholeDiskName.MatchString(fields[2]) {
			continue
		}
		sectorsRead, err1 := strconv.ParseUint(fields[5], 10, 64)
		sectorsWritten, err2 := strconv.ParseUint(fields[9], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		read += float64(sectorsRead) * 512
		written += float64(sectorsWritten) * 512
		any = true
	}
	return read, written, any
}
