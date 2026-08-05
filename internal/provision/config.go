package provision

import (
	"bytes"
	"encoding/json"
)

// The jail's firecracker.json. The boot-source / drives / network-interfaces /
// machine-config shape is the shell heredoc's, with jail-RELATIVE host paths
// (vmlinux, rootfs.ext4) that the jailed process resolves after its chroot.
//
// These are structs rather than a map because the Python builds an ordered dict
// and dumps it: Go sorts a map's keys and would reorder every object. The field
// order below IS the file's key order.
type firecrackerFile struct {
	BootSource        bootSource           `json:"boot-source"`
	Drives            []drive              `json:"drives"`
	NetworkInterfaces []networkInterface   `json:"network-interfaces"`
	MachineConfig     machineConfiguration `json:"machine-config"`
	// MMDSConfig is on EVERY VM. It is inert unless something PUTs data (warm
	// clones stage an identity payload; an ordinary VM serves nothing) — but it must
	// be in the GOLDEN's boot config for the captured vmstate to carry the
	// MMDS-enabled net device, and a uniform config keeps every VM bakeable. V1
	// pinned: the freshen unit does a plain GET, no session tokens.
	MMDSConfig mmdsConfiguration `json:"mmds-config"`
	// Entropy is virtio-rng: it feeds the guest kernel a hardware RNG so it seeds
	// its CSPRNG from the host's entropy. Ubuntu ships CONFIG_HW_RANDOM_VIRTIO=m, so
	// the driver is not in the extracted vmlinux — sync-image bakes the virtio_rng
	// module into the rootfs and pins it in modules-load.d, and only then does the
	// device bind (/dev/hwrng). No rate limiter: entropy is cheap and we want it as
	// fast as the guest asks. Like mmds-config it must be in the GOLDEN's boot
	// config so the captured vmstate carries the device.
	Entropy struct{} `json:"entropy"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	// BootArguments disables the guest 8250 serial device at boot
	// (prod-host-setup.md "8250 Serial Device"): the device is tied to
	// Firecracker's stdout, and a guest with serial access can drive unbounded host
	// log and storage growth. `console=ttyS0` is deliberately NOT passed either —
	// the guest's console writes would otherwise flood firecracker.log. The host
	// side is bounded too (the systemd unit logrotates the per-VM log), and since
	// the guest can technically re-enable the device after boot, that bounded
	// storage is the load-bearing half of the mitigation. reboot=k and panic=1 leave
	// the guest's reboot and panic behaviour unchanged.
	BootArguments string `json:"boot_args"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type networkInterface struct {
	InterfaceID    string `json:"iface_id"`
	GuestMAC       string `json:"guest_mac"`
	HostDeviceName string `json:"host_dev_name"`
}

type machineConfiguration struct {
	VCPUCount           int `json:"vcpu_count"`
	MemorySizeMebibytes int `json:"mem_size_mib"`
}

type mmdsConfiguration struct {
	Version           string   `json:"version"`
	NetworkInterfaces []string `json:"network_interfaces"`
}

const bootArguments = "8250.nr_uarts=0 reboot=k panic=1"

// firecrackerConfiguration renders the jail's firecracker.json.
func firecrackerConfiguration(params Params) (string, error) {
	file := firecrackerFile{
		BootSource: bootSource{KernelImagePath: "vmlinux", BootArguments: bootArguments},
		Drives: []drive{
			{DriveID: "rootfs", PathOnHost: "rootfs.ext4", IsRootDevice: true, IsReadOnly: false},
		},
		NetworkInterfaces: []networkInterface{
			{InterfaceID: "eth0", GuestMAC: params.MACAddress, HostDeviceName: params.TapDevice},
		},
		MachineConfig: machineConfiguration{
			VCPUCount: params.VCPUs, MemorySizeMebibytes: params.MemoryMB,
		},
		MMDSConfig: mmdsConfiguration{Version: "V1", NetworkInterfaces: []string{"eth0"}},
	}
	// The data disk is a second, non-root drive (the guest's /dev/vdb), resolved
	// post-chroot to the data.ext4 node. Appended only when the VM has one, so an
	// ordinary VM's config is byte-identical to what it was before data disks
	// existed.
	if params.DataDiskGB > 0 {
		file.Drives = append(file.Drives, drive{
			DriveID: "data", PathOnHost: "data.ext4", IsRootDevice: false, IsReadOnly: false,
		})
	}
	return encodeJSON(file, "  ")
}

// encodeJSON renders value the way Python's json.dumps(…, indent=n) does, which is
// what makes the generated files byte-identical to the Python's.
//
// Two settings carry that, and neither is cosmetic. HTML escaping is OFF because
// Go's default turns `<`, `>` and `&` into < and friends while Python leaves
// them alone — an SSH key comment or a routing URL with a `&` in it would differ.
// An Encoder is used rather than MarshalIndent because Encode appends exactly the
// one trailing newline the Python adds by hand.
func encodeJSON(value any, indent string) (string, error) {
	var rendered bytes.Buffer
	encoder := json.NewEncoder(&rendered)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", indent)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return rendered.String(), nil
}
