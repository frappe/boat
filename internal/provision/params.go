package provision

// The verb's inputs and its one reported fact — scripts/provision-vm.py's
// ProvisionInputs, rendered as a Go struct so a reader can hold the Python
// dataclass and this file side by side and see the same list.

// defaultResourceArguments is the jailer `--resource-limit` fallback for a
// break-glass hand run that omits --resource-arg. The controller always passes
// the VM's full triple (atlas.networking.resource_limit_args), but that is
// effectively the constant `no-file=1024` today, so an operator need not type it.
// A VALUE, not a flag — the launcher owns the `--resource-limit` prefix; 1024
// mirrors networking.MAX_OPEN_FILES.
var defaultResourceArguments = []string{"no-file=1024"}

// Params is scripts/provision-vm.py's ProvisionInputs, minus the two fields that
// belong to the identity write rather than to provision: preserve_host_keys and
// data_disk_mount_at shape the `inject` callback the caller hands in, and nothing
// here branches on either. Every other field is one --kebab-case flag on `boat
// provision-vm`.
//
// The block from MACAddress to NamespaceVeth is DERIVED — every value is
// controller-allocated and none is defaultable, because a wrong one mis-wires the
// jail or the network. They stay required; the break-glass ergonomic is that each
// names the atlas.networking function that computes it (see spec/06).
type Params struct {
	VirtualMachineName string // UUID; directory, tap, systemd instance, identity
	ImageName          string // directory under /var/lib/atlas/images
	KernelFilename     string // filename inside the image directory
	RootfsFilename     string // filename inside the image directory
	VCPUs              int
	MemoryMB           int
	DiskGB             int    // final rootfs size for this VM
	SSHPublicKey       string // injected into the rootfs, and into a warm clone's MMDS

	MACAddress         string // derived: atlas.networking.derive_mac(uuid)
	TapDevice          string // derived: atlas.networking.derive_tap(uuid)
	VirtualMachineIPv6 string // controller-allocated: atlas.networking.allocate_ipv6(server)
	IPv4HostCIDR       string // derived: derive_ipv4_link(ipv6)[0], host side of the NAT44 /30
	IPv4GuestCIDR      string // derived: derive_ipv4_link(ipv6)[1], guest side of the /30
	IPv4Gateway        string // the host side without its mask, the guest's v4 gateway
	FirecrackerUID     int    // derived: atlas.networking.derive_uid(uuid); gid == uid
	Namespace          string // derived: atlas.networking.derive_netns(uuid)
	HostVeth           string // derived: derive_veth_pair(uuid)[0]
	NamespaceVeth      string // derived: derive_veth_pair(uuid)[1]

	// CgroupArguments are jailer cgroup VALUES, one per repeated --cgroup-arg
	// flag; the launcher prefixes each with --cgroup. Values only, not the
	// interleaved "--cgroup <v>" the shell's ATLAS_CGROUP_ARGS held, because a
	// literal "--cgroup" token in an append-list collides with flag parsing and the
	// prefix is a constant the launcher owns anyway. This is what kills the shell's
	// mapfile hack: a value with an internal space (cpu.max's "<quota> <period>")
	// is one argv token, so nothing word-splits it.
	//
	// REQUIRED: it encodes the VM's real memory/cpu limits, and an empty cgroup set
	// would silently un-bound the VM, so failing loud is correct.
	CgroupArguments []string
	// ResourceArguments are --resource-limit VALUES. Empty falls back to
	// defaultResourceArguments at use, so a hand run can omit the flag; the
	// controller always passes the full set and its path is unaffected.
	ResourceArguments []string

	// SnapshotRootfsPath is a snapshot's /dev/atlas/<name> device path — the clone
	// path. Empty provisions from the base image.
	SnapshotRootfsPath string
	// RoutingBaseURL is the Atlas controller base URL the in-guest routing client
	// POSTs to (spec/18). Carried here for the warm clone's MMDS payload; the cold
	// path writes it through the identity injection. Empty for a VM whose
	// controller did not inject one — the guest client then no-ops. NON-SECRET.
	RoutingBaseURL string
	// ReservedIPv4 is a Reserved IP attached to this VM, empty for every ordinary
	// one. Carried so a rebuild of a VM that already has an attached v4 re-creates
	// its 1:1-NAT on first boot; live attach/detach goes through its own verb.
	ReservedIPv4 string
	// PrivateAddress and TenantPrefix are the VM's private-plane identity on the
	// WireGuard host mesh (spec/25): the derived fdaa:: /128 and its tenant /48.
	// Both empty for a VM with no tenant — vm-network-up gates the whole private
	// block on both, so an ordinary VM's env is unchanged.
	PrivateAddress string
	TenantPrefix   string

	// DataDiskGB is the optional second writable disk (the guest's /dev/vdb); 0
	// means none. DataDiskFormat is an int rather than a bool for two reasons that
	// agree: the Task runner renders a bool as a truthy string, so "0" would read
	// True, and Go's flag package only accepts --flag=0 for a bool while the
	// controller renders "--data-disk-format 0". An int parses cleanly either way.
	//
	// Its DEFAULT IS 1, and Go's zero value is 0, which means the opposite thing —
	// attach the volume raw, with no filesystem. The caller carries that default
	// (the CLI's flag declares it); a Params built by hand and left at zero gets an
	// unformatted disk the guest cannot mount.
	DataDiskGB     int
	DataDiskFormat int
	// DataSnapshotRootfsPath seeds the data disk from a data-disk snapshot LV
	// (clone); empty means a fresh blank one.
	DataSnapshotRootfsPath string

	// CloneRootfsDevice is /dev/mapper/atlas-vm-<uuid>-clone: boot-then-hydrate
	// migration (spec/24 §0). The guest boots on the dm-clone read-through device
	// BEFORE hydration finishes, so it serves while blocks copy in behind it. With
	// it set the disk LV is NOT (re)created (the dest LV exists and is hydrating
	// live; a snapshot would race the read-through), identity is NOT injected here
	// (it was already injected THROUGH the clone — mounting the plain LV would
	// fault), and the jail rootfs node is mknod'd at the CLONE. At CollapseClone the
	// clone table is reloaded onto the same dest LV, keeping that node valid.
	CloneRootfsDevice string
	// WarmSnapshotDirectory is the durable directory holding a warm golden's
	// vmstate.bin/mem.bin/host-signature.json, paired with SnapshotRootfsPath (which
	// must be that golden's disk snapshot). When set the clone's disk is a bare CoW
	// of the golden — no grow, no UUID reroll, no identity injection, because the
	// frozen RAM's filesystem cache must keep matching the disk — the golden pair is
	// hard-linked into the jail behind a READY marker, and the clone's identity is
	// staged as MMDS metadata for the in-guest freshen unit.
	WarmSnapshotDirectory string
}

// Result reports whether a warm memory pair was staged into this VM's jail. A
// warm clone whose disk already existed and had been booted gets none, and its
// next start therefore cold-boots — a fact the operator is told rather than left
// to infer from a missing marker.
type Result struct{ WarmPairStaged bool }
