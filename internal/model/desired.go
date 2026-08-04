package model

import "time"

// DesiredPower is the only power input Boat's reconciler takes.
//
// An explicit PowerStopped outranks a wake trap: a VM that was told to stop is
// not woken by traffic. That precedence is a rule, not an implementation
// detail — without it a stopped VM comes back to life the moment anyone probes
// its address, and the operator who stopped it has no way to make it stay down.
type DesiredPower string

const (
	PowerRunning DesiredPower = "Running"
	PowerStopped DesiredPower = "Stopped"
)

// DesiredVirtualMachine is what Atlas wants of one VM. Boat stores it, diffs
// against what it observes, and runs forward to converge.
//
// Everything here is a number, an address or an enrolment. Nothing says who
// owns the VM or what runs inside it, which is what keeps Boat workload-
// agnostic while still being able to realize the whole spec.
type DesiredVirtualMachine struct {
	UUID string `json:"uuid"`
	// BootEpoch is the fence. Atlas is its sole issuer and it never goes
	// backwards; Boat refuses to boot a UUID it holds no epoch for.
	BootEpoch    int64        `json:"boot_epoch"`
	DesiredPower DesiredPower `json:"desired_power"`

	// Server is the host Atlas has placed this VM on — its Frappe Server name, not
	// a tenant or a workload, so it stays within Boat's host-not-owner remit. It is
	// the §11.1 "server == self" discriminator: a host boots a VM only when the
	// desired record names THIS host, which refuses a VM Atlas has assigned
	// elsewhere even at a valid epoch. Empty means Atlas asserted no placement (an
	// older controller, or a host that has not been told its own name), and the gate
	// then falls back to the epoch alone — see internal/api/fence.go.
	Server string `json:"server"`

	VCPUs             int     `json:"vcpus"`
	CPUMaxCores       float32 `json:"cpu_max_cores"`
	CPUMode           string  `json:"cpu_mode"`
	MemoryMegabytes   int     `json:"memory_megabytes"`
	DiskGigabytes     int     `json:"disk_gigabytes"`
	DataDiskGigabytes int     `json:"data_disk_gigabytes"`

	// SleepOnIdle is enrolment, not policy: Boat runs the reflex, Atlas decides
	// which VMs are subject to it.
	SleepOnIdle        bool `json:"sleep_on_idle"`
	IdleTimeoutSeconds int  `json:"idle_timeout_seconds"`

	IPv6Address    string `json:"ipv6_address"`
	PrivateAddress string `json:"private_address"`
	MACAddress     string `json:"mac_address"`
	TapDevice      string `json:"tap_device"`

	AssertedAt time.Time `json:"asserted_at"`
}

// UnitLiveness is one sibling systemd unit as this host reports it.
type UnitLiveness struct {
	Name        string `json:"name"`
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
}

// LogicalVolume is one LV in the host's inventory. Origin is set when the
// volume is a snapshot or a clone of another, which is how a half-collapsed
// migration is spotted.
type LogicalVolume struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	Pool      string `json:"pool"`
	Origin    string `json:"origin"`
}

// HostFacts is what this host is, as observed now rather than as recorded at
// bootstrap. Capacity that drifts silently is capacity that overcommits.
type HostFacts struct {
	Hostname             string `json:"hostname"`
	BoatVersion          string `json:"boat_version"`
	KernelVersion        string `json:"kernel_version"`
	FirecrackerVersion   string `json:"firecracker_version"`
	VCPUsTotal           int    `json:"vcpus_total"`
	MemoryMegabytesTotal int    `json:"memory_megabytes_total"`
	MemoryMegabytesFree  int    `json:"memory_megabytes_free"`
	// Pointers, because an unmeasured fact must be ABSENT and not zero. Atlas
	// treats a zero pool total as "unmeasured" and an unmeasured axis as
	// UNLIMITED (api/server_capacity.py: `if s.get(...)` is falsy for 0, so the
	// axis total becomes None and placement stops gating it). Reporting 0 for a
	// pool this host could not read would therefore let Atlas keep packing VMs
	// onto a host whose disk it cannot see — strictly worse than the loud
	// failure it replaced.
	PoolDiskGigabytesTotal *int     `json:"pool_disk_gigabytes_total,omitempty"`
	PoolUsedPercent        *float32 `json:"pool_used_percent,omitempty"`
	HostSignature          string   `json:"host_signature"`
}

// Export is Boat's entire observed state in one document — the call that lets
// Atlas rebuild its mirror in a single transaction.
//
// ObservedEpoch is what makes it usable for more than display: a CAS write can
// be matched against it, so a caller acts on exactly the snapshot it read.
type Export struct {
	ObservedEpoch   int64            `json:"observed_epoch"`
	TakenAt         time.Time        `json:"taken_at"`
	Host            HostFacts        `json:"host"`
	VirtualMachines []VirtualMachine `json:"virtual_machines"`
	Units           []UnitLiveness   `json:"units"`
	LogicalVolumes  []LogicalVolume  `json:"logical_volumes"`
	FenceEpochs     map[string]int64 `json:"fence_epochs"`
	// Quarantined is what this host holds that could not be read as a VM. It
	// belongs in the export because it is the state an operator most needs to
	// see and the one Atlas can least afford to infer: a half-terminated VM is
	// invisible from the VM list by construction, so a host reporting no VMs and
	// a host reporting no VMs plus three quarantined artifact sets are the same
	// document without it.
	Quarantined []Quarantine `json:"quarantine"`
}
