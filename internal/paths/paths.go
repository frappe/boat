// Package paths derives every on-host path for one VM from its UUID. Pure
// string derivation: no host access, no errors, unit-testable with no host.
//
// The jail path nests the UUID twice — <vm>/jail/firecracker/<uuid>/root —
// because the jailer chroots into <chroot-base>/firecracker/<id>/root and the
// chroot base it is given is already the per-VM directory. Six shell scripts
// (provision, rebuild, resize, pause, resume, terminate) each rebuilt that
// layout inline before it was derived in one place.
//
// VirtualMachine also owns the Firecracker API-socket workaround: the absolute
// socket path is longer than AF_UNIX allows, so callers cd into its directory
// and address it by a short relative name. Both halves are exposed here so no
// caller has to reassemble the trick by hand.
//
// Atlas's venv constants are deliberately not carried over — they name the
// Python interpreter Boat exists to retire.
package paths

const (
	// AtlasRoot and the tree below it keep Atlas's names: Boat adopts hosts whose
	// VMs already live at these paths, so the layout is inherited, not chosen.
	AtlasRoot                = "/var/lib/atlas"
	ImagesDirectory          = AtlasRoot + "/images"
	VirtualMachinesDirectory = AtlasRoot + "/virtual-machines"
	BinDirectory             = AtlasRoot + "/bin"
	// SnapshotsDirectory holds the durable warm-snapshot artifacts — the golden
	// vmstate/memory pair and the host signature — one directory per snapshot
	// record. It sits outside every VM directory deliberately: a golden outlives
	// the VM that built it and is hard-linked into N clone jails, so terminate's
	// rm -rf of a VM tree must not be able to take it.
	SnapshotsDirectory = AtlasRoot + "/snapshots"
)

// The jailed Firecracker resolves the paths in its API bodies after the chroot,
// so snapshot and metadata files are named relative to the jail root — the same
// convention firecracker.json already uses for rootfs.ext4 and vmlinux.
const (
	MemorySnapshotVMStateInJail = "snapshot/vmstate.bin"
	MemorySnapshotMemoryInJail  = "snapshot/mem.bin"
	MetadataInJail              = "metadata.json"
)

// SunPathMax is the AF_UNIX sun_path limit in bytes, NUL included. A jailed VM's
// absolute API-socket path runs well past it, which is the entire reason for the
// relative-cd dance around APISocketDirectory and APISocketName.
const SunPathMax = 108

// VirtualMachine is every on-host path for one VM, derived from its UUID.
type VirtualMachine struct{ UUID string }

// ForVirtualMachine returns the path set for the VM with this UUID.
func ForVirtualMachine(uuid string) VirtualMachine { return VirtualMachine{UUID: uuid} }

// Directory is the VM's root directory; removing it takes the whole jail tree.
func (virtualMachine VirtualMachine) Directory() string {
	return VirtualMachinesDirectory + "/" + virtualMachine.UUID
}

func (virtualMachine VirtualMachine) LogDirectory() string {
	return virtualMachine.Directory() + "/log"
}

// NetworkEnvironment is the sidecar carrying tap/netns/veth/uid. The network and
// disk systemd hooks read it, which is what lets a host rebuild a VM's
// networking after a reboot without reaching back to the Frappe database.
func (virtualMachine VirtualMachine) NetworkEnvironment() string {
	return virtualMachine.Directory() + "/network.env"
}

// FirewallEnvironment is the sidecar carrying this VM's public-ingress firewall
// (spec/20-firewall.md): its IPv6 and the allowed proto/port list. The
// network-up hook replays it into the nft public_filter block at cold boot, and
// the firewall verb writes and removes it. It lives inside the VM tree so
// terminate's rm -rf sweeps it. Absent for a VM with no firewall, which stays
// fully public.
func (virtualMachine VirtualMachine) FirewallEnvironment() string {
	return virtualMachine.Directory() + "/firewall.env"
}

// TunnelsDirectory holds this VM's WireGuard tunnel sidecars
// (spec/19-vpn-broker.md). Network-up re-applies every tunnel here at cold boot;
// the tunnel verb writes or removes one at a time. Inside the VM tree so
// terminate sweeps it with the rest of the VM's host state.
func (virtualMachine VirtualMachine) TunnelsDirectory() string {
	return virtualMachine.Directory() + "/tunnels"
}

// TunnelEnvironment is the KEY=value metadata sidecar for one tunnel (0644).
func (virtualMachine VirtualMachine) TunnelEnvironment(tunnelName string) string {
	return virtualMachine.TunnelsDirectory() + "/" + tunnelName + ".env"
}

// TunnelKey is the 0600 file holding the tunnel's host private key. `wg set`
// reads a key from a path and never from its command line, so the secret stays
// off the process table and out of the operation's audit record.
func (virtualMachine VirtualMachine) TunnelKey(tunnelName string) string {
	return virtualMachine.TunnelsDirectory() + "/" + tunnelName + ".key"
}

// JailChrootBase is what the jailer's --chroot-base-dir points at.
func (virtualMachine VirtualMachine) JailChrootBase() string {
	return virtualMachine.Directory() + "/jail"
}

// JailRoot is the directory the jailed Firecracker sees as `/`. The UUID appears
// twice — <directory>/jail/firecracker/<uuid>/root — because the jailer appends
// firecracker/<id>/root to a chroot base that is itself already per-VM.
func (virtualMachine VirtualMachine) JailRoot() string {
	return virtualMachine.JailChrootBase() + "/firecracker/" + virtualMachine.UUID + "/root"
}

// RootFilesystemNode is the block-special node Firecracker opens as its rootfs;
// jail-relative, that is `rootfs.ext4`.
func (virtualMachine VirtualMachine) RootFilesystemNode() string {
	return virtualMachine.JailRoot() + "/rootfs.ext4"
}

// DataNode is the block-special node Firecracker opens as the data disk — the
// guest's /dev/vdb and the peer of RootFilesystemNode. Only present when the VM
// has a data disk.
func (virtualMachine VirtualMachine) DataNode() string {
	return virtualMachine.JailRoot() + "/data.ext4"
}

func (virtualMachine VirtualMachine) Kernel() string {
	return virtualMachine.JailRoot() + "/vmlinux"
}

func (virtualMachine VirtualMachine) FirecrackerConfig() string {
	return virtualMachine.JailRoot() + "/firecracker.json"
}

func (virtualMachine VirtualMachine) JailerLaunch() string {
	return virtualMachine.Directory() + "/jailer-launch.sh"
}

// MemorySnapshotDirectory is where a full memory-state snapshot (vmstate plus
// guest RAM) lands. It is inside the jail so the jailed Firecracker can write it
// and so terminate's rm -rf sweeps it: the snapshot-stop verb writes it, restore
// consumes it.
func (virtualMachine VirtualMachine) MemorySnapshotDirectory() string {
	return virtualMachine.JailRoot() + "/snapshot"
}

// MemorySnapshotMarker is present exactly when the vmstate/memory pair below is
// complete and still matches the disk. It is written last on stop and removed
// before the resume that consumes it, so a half-written pair can never be
// mistaken for a restorable one. The launcher keys off it: present means start
// Firecracker idle, with no --config-file, for restore to load into; absent
// means an ordinary cold boot.
func (virtualMachine VirtualMachine) MemorySnapshotMarker() string {
	return virtualMachine.MemorySnapshotDirectory() + "/READY"
}

func (virtualMachine VirtualMachine) MemorySnapshotVMState() string {
	return virtualMachine.MemorySnapshotDirectory() + "/vmstate.bin"
}

func (virtualMachine VirtualMachine) MemorySnapshotMemory() string {
	return virtualMachine.MemorySnapshotDirectory() + "/mem.bin"
}

// MemorySnapshotSignature is the host signature captured with a warm golden
// snapshot and staged beside the marker at provision. Restore compares it to the
// live host before loading, because a snapshot is only restorable on the
// CPU/kernel/Firecracker it was captured on and a mismatch has to cold-boot
// instead. Absent for a same-VM stop/start pair, which is on the same host by
// construction.
func (virtualMachine VirtualMachine) MemorySnapshotSignature() string {
	return virtualMachine.MemorySnapshotDirectory() + "/host-signature.json"
}

// MetadataFile is the MMDS payload staged for a warm clone: the clone's own
// identity (addresses, hostname, machine-id, SSH key), served to the guest at
// 169.254.169.254 so its in-guest freshen unit can adopt it after a restore. A
// clone's disk is never mutated offline — the frozen RAM's filesystem cache has
// to keep matching it — which leaves MMDS as the only identity channel. Absent
// for every ordinary VM.
func (virtualMachine VirtualMachine) MetadataFile() string {
	return virtualMachine.JailRoot() + "/metadata.json"
}

// APISocketDirectory holds firecracker.socket. Callers cd here — as root, since
// it is 0700 and owned by the per-VM uid — and address the socket by
// APISocketName, which is how the connect stays under SunPathMax.
func (virtualMachine VirtualMachine) APISocketDirectory() string {
	return virtualMachine.JailRoot() + "/run"
}

// APISocket is the absolute socket path, for stat and existence checks only:
// stat has no length limit. Never hand this to curl --unix-socket; use
// APISocketDirectory together with APISocketName.
func (virtualMachine VirtualMachine) APISocket() string {
	return virtualMachine.APISocketDirectory() + "/" + virtualMachine.APISocketName()
}

// APISocketName is the short relative name to connect to after cd-ing into
// APISocketDirectory.
func (virtualMachine VirtualMachine) APISocketName() string {
	return "firecracker.socket"
}

// SleepingMarker is present while the VM sleeps: it suppresses systemd
// auto-start after a host reboot (the unit's ConditionPathExists=! condition) and is the
// authority for the Sleeping status. Sleep writes it after the stop; wake
// removes it before the start. It lives outside the jail, in the VM directory,
// so terminate's rm -rf still sweeps it.
func (virtualMachine VirtualMachine) SleepingMarker() string {
	return virtualMachine.Directory() + "/sleeping"
}

// TrafficCounterFile is the last-seen nftables byte total for this VM, stored as
// JSON {"bytes": N} so a per-minute poll computes its delta on the host and
// reports only whether the VM is active.
func (virtualMachine VirtualMachine) TrafficCounterFile() string {
	return virtualMachine.Directory() + "/traffic-counter.json"
}

// SystemdUnit is the per-VM systemd instance name.
func (virtualMachine VirtualMachine) SystemdUnit() string {
	return "firecracker-vm@" + virtualMachine.UUID + ".service"
}

// ImageDirectory is where one image's artifacts live.
func ImageDirectory(imageName string) string {
	return ImagesDirectory + "/" + imageName
}

// WarmSnapshotDirectory is the durable artifact directory of one warm snapshot:
// the vmstate/memory pair captured at the paused instant, plus
// host-signature.json. Provision hard-links that pair into each clone's jail, so
// N clones copy-on-write share one read-only memory file; deleting the snapshot
// record removes this directory.
func WarmSnapshotDirectory(snapshotName string) string {
	return SnapshotsDirectory + "/" + snapshotName
}

// IsUUID reports whether name is the 8-4-4-4-12 lowercase-hex shape every VM on
// a host is named by.
//
// It exists because a UUID is not merely an identifier here — it is a path
// segment, spliced into every command the daemon runs against a VM, and matched
// by a `*` in the sudo allow-list. A `*` in a sudoers argument matches `/` and
// `.` and spaces, so a name containing `..` walks out of /var/lib/atlas and a
// name containing a space can add arguments to the command it lands in. The
// allow-list cannot express "and this segment is a UUID"; this can, and it runs
// before the name is ever rendered.
//
// So the rule is: a name that fails this never becomes a path. Anything that
// learns names from the host rather than from Atlas — the adoption scan reads
// them out of a directory listing — has to check.
func IsUUID(name string) bool {
	const shape = "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
	if len(name) != len(shape) {
		return false
	}
	for index := range len(shape) {
		character, expected := name[index], shape[index]
		if expected == '-' {
			if character != '-' {
				return false
			}
			continue
		}
		isHex := (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')
		if !isHex {
			return false
		}
	}
	return true
}
