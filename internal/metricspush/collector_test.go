package metricspush

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/frappe/boat/internal/datum"
	"github.com/frappe/boat/internal/metrics"
	"github.com/frappe/boat/internal/model"
)

// byMetric indexes a sample slice by metric name. If a metric appears more than
// once, the last sample wins.
func byMetric(samples []datum.Sample) map[string]datum.Sample {
	index := make(map[string]datum.Sample, len(samples))
	for _, s := range samples {
		index[s.Metric] = s
	}
	return index
}

func TestHostSamplesSkipUnmeasured(t *testing.T) {
	host := metrics.Metrics{
		CPUCores:            8,
		MemoryBytes:         -1,
		DiskFreeBytes:       100,
		PoolDataPercent:     -1,
		PoolMetadataPercent: 12.5,
		VirtualMachines:     99, // ignored: host_virtual_machines comes from the VM list
		FirecrackerRunning:  2,
	}
	grouped := Collect("2026-08-06T00:00:00Z", "srv", host, make([]model.VirtualMachine, 3), DefaultRoots())

	index := byMetric(grouped.Host)
	want := map[string]float64{
		"host_up":                    1,
		"host_cpu_cores":             8,
		"host_disk_free_bytes":       100,
		"host_pool_metadata_percent": 12.5,
		"host_virtual_machines":      3,
		"host_firecracker_running":   2,
	}
	for metric, value := range want {
		s, ok := index[metric]
		if !ok {
			t.Errorf("host samples missing %q", metric)
			continue
		}
		if s.Value != value {
			t.Errorf("%s = %v, want %v", metric, s.Value, value)
		}
	}
	for _, metric := range []string{"host_memory_bytes", "host_pool_data_percent"} {
		if _, ok := index[metric]; ok {
			t.Errorf("host samples contain %q, want it skipped (unmeasured)", metric)
		}
	}
	for _, s := range grouped.Host {
		if s.Labels["server"] != "srv" {
			t.Errorf("host sample %s server label = %q, want %q", s.Metric, s.Labels["server"], "srv")
		}
	}
}

func TestVMRunningReadsCgroupAndNet(t *testing.T) {
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

	grouped := Collect("2026-08-06T00:00:00Z", "srv", metrics.Metrics{}, []model.VirtualMachine{
		{UUID: uuid, ObservedStatus: model.StatusRunning},
	}, roots)

	samples, ok := grouped.VMs[uuid]
	if !ok {
		t.Fatalf("no samples grouped under VM %q", uuid)
	}
	index := byMetric(samples)

	want := map[string]float64{
		"vm_up":                           1,
		"vm_memory_current_bytes":         1048576,
		"vm_memory_max_bytes":             2097152,
		"vm_cpu_usage_seconds_total":      2.5,
		"vm_io_read_bytes_total":          4096,
		"vm_io_write_bytes_total":         8192,
		"vm_network_transmit_bytes_total": 111, // host rx = guest egress
		"vm_network_receive_bytes_total":  222, // host tx = guest ingress
	}
	for metric, value := range want {
		s, ok := index[metric]
		if !ok {
			t.Errorf("VM samples missing %q", metric)
			continue
		}
		if s.Value != value {
			t.Errorf("%s = %v, want %v", metric, s.Value, value)
		}
	}

	if up := index["vm_up"]; up.Labels["status"] != "Running" {
		t.Errorf("vm_up status label = %q, want %q", up.Labels["status"], "Running")
	}
	if up := index["vm_up"]; up.Labels["vm"] != uuid {
		t.Errorf("vm_up vm label = %q, want %q", up.Labels["vm"], uuid)
	}
	for _, s := range samples {
		if s.Metric == "vm_up" {
			continue
		}
		if s.Labels["vm"] != uuid {
			t.Errorf("%s vm label = %q, want %q", s.Metric, s.Labels["vm"], uuid)
		}
		if s.Labels["server"] != "srv" {
			t.Errorf("%s server label = %q, want %q", s.Metric, s.Labels["server"], "srv")
		}
		if _, hasUUID := s.Labels["uuid"]; hasUUID {
			t.Errorf("%s carries a uuid label, which datum rejects", s.Metric)
		}
	}
}

func TestVMNotRunningIsUpZeroOnly(t *testing.T) {
	roots, _, _ := fakeRoots(t)
	uuid := "6d0a1b2c-0000-0000-0000-000000000000"

	grouped := Collect("2026-08-06T00:00:00Z", "srv", metrics.Metrics{}, []model.VirtualMachine{
		{UUID: uuid, ObservedStatus: model.StatusStopped},
	}, roots)

	samples, ok := grouped.VMs[uuid]
	if !ok {
		t.Fatalf("no samples grouped under VM %q", uuid)
	}
	if len(samples) != 1 {
		t.Fatalf("VM samples = %d, want exactly 1 (vm_up only)", len(samples))
	}
	up := samples[0]
	if up.Metric != "vm_up" {
		t.Errorf("only sample metric = %q, want %q", up.Metric, "vm_up")
	}
	if up.Value != 0 {
		t.Errorf("vm_up = %v, want 0", up.Value)
	}
	if up.Labels["status"] != "Stopped" {
		t.Errorf("vm_up status label = %q, want %q", up.Labels["status"], "Stopped")
	}
}

func TestMemoryMaxLiteralSkipped(t *testing.T) {
	roots, cgroup, sysClassNet := fakeRoots(t)
	uuid := "6d0a1b2c-0000-0000-0000-000000000000"

	vmDir := filepath.Join(cgroup, "firecracker", uuid)
	mustMkdirAll(t, vmDir)
	mustWriteFile(t, filepath.Join(vmDir, "memory.current"), "1048576\n")
	mustWriteFile(t, filepath.Join(vmDir, "memory.max"), "max\n")
	mustWriteFile(t, filepath.Join(vmDir, "cpu.stat"), "usage_usec 2500000\nuser_usec 1\nsystem_usec 1\n")
	mustWriteFile(t, filepath.Join(vmDir, "io.stat"), "259:0 rbytes=4096 wbytes=8192 rios=1 wios=2\n")

	statsDir := filepath.Join(sysClassNet, "atlas-h6d0a1b2", "statistics")
	mustMkdirAll(t, statsDir)
	mustWriteFile(t, filepath.Join(statsDir, "rx_bytes"), "111\n")
	mustWriteFile(t, filepath.Join(statsDir, "tx_bytes"), "222\n")

	grouped := Collect("2026-08-06T00:00:00Z", "srv", metrics.Metrics{}, []model.VirtualMachine{
		{UUID: uuid, ObservedStatus: model.StatusRunning},
	}, roots)

	index := byMetric(grouped.VMs[uuid])
	if _, ok := index["vm_memory_max_bytes"]; ok {
		t.Error("vm_memory_max_bytes present, want it skipped for an unlimited (max) cgroup")
	}
	for _, metric := range []string{
		"vm_up",
		"vm_memory_current_bytes",
		"vm_cpu_usage_seconds_total",
		"vm_io_read_bytes_total",
		"vm_network_transmit_bytes_total",
	} {
		if _, ok := index[metric]; !ok {
			t.Errorf("VM samples missing %q", metric)
		}
	}
}

func TestHostUtilizationSamples(t *testing.T) {
	dir := t.TempDir()
	proc := filepath.Join(dir, "proc")
	if err := os.MkdirAll(filepath.Join(proc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(proc, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("meminfo", "MemTotal:       16384 kB\nMemAvailable:    4096 kB\n")
	write("stat", "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3 4\n") // busy=total(1000)-idle(700)-iowait(100)=200 -> 2.0s
	write("net/dev", "Inter-|   Receive\n face |bytes\n  eth0: 1000 1 2 3 4 5 6 7 2000 8\n    lo: 9 0 0 0 0 0 0 0 9 0\n  veth9: 5 0 0 0 0 0 0 0 5 0\n")
	write("diskstats", " 8 0 sda 1 0 10 0 1 0 20 0 0 0 0\n 8 1 sda1 1 0 999 0 0 0 0 0 0 0 0\n")

	roots := Roots{Proc: proc, Cgroup: filepath.Join(dir, "cg"), SysClassNet: filepath.Join(dir, "net")}
	index := byMetric(hostUtilizationSamples("2026-08-07T00:00:00Z", "srv", roots))

	want := map[string]float64{
		"host_memory_used_bytes":            (16384 - 4096) * 1024,
		"host_memory_available_bytes":       4096 * 1024,
		"host_cpu_usage_seconds_total":      2.0,
		"host_network_receive_bytes_total":  1000, // eth0 only (lo, veth excluded)
		"host_network_transmit_bytes_total": 2000,
		"host_disk_read_bytes_total":        10 * 512, // sda only, not sda1
		"host_disk_write_bytes_total":       20 * 512,
	}
	for metric, value := range want {
		sample, ok := index[metric]
		if !ok {
			t.Errorf("missing %q", metric)
			continue
		}
		if sample.Value != value {
			t.Errorf("%s = %v, want %v", metric, sample.Value, value)
		}
	}
	for _, sample := range hostUtilizationSamples("2026-08-07T00:00:00Z", "srv", roots) {
		if sample.Labels["server"] != "srv" {
			t.Errorf("util sample %s server label = %q, want %q", sample.Metric, sample.Labels["server"], "srv")
		}
	}
}

// fakeRoots builds a temp tree laid out like the real host and returns the Roots
// that point at it, plus the cgroup and sysfs net roots for convenience.
func fakeRoots(t *testing.T) (roots Roots, cgroup, sysClassNet string) {
	t.Helper()
	base := t.TempDir()
	cgroup = filepath.Join(base, "cgroup")
	sysClassNet = filepath.Join(base, "sys", "class", "net")
	mustMkdirAll(t, cgroup)
	mustMkdirAll(t, sysClassNet)
	return Roots{Cgroup: cgroup, Proc: filepath.Join(base, "proc"), SysClassNet: sysClassNet}, cgroup, sysClassNet
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
