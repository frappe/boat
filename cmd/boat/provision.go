package main

import (
	"context"
	"fmt"
	"io"

	"github.com/frappe/boat/internal/hostkeys"
	"github.com/frappe/boat/internal/provision"
	"github.com/frappe/boat/internal/run"
	"github.com/frappe/boat/internal/vm"
)

// routingEnvironmentPath is the one guest file provision writes that is neither an
// address nor a key: the controller base URL the in-guest routing client POSTs to
// (spec/18). It crosses into the guest as an opaque {path, content} pair, because a
// field named for what the file MEANS would put a service semantic in Boat's
// vocabulary — see vm.Identity.ExtraEnvironment.
const routingEnvironmentPath = "/etc/atlas-routing.env"

// provisionVM creates one VM on this host and starts it — `boat provision-vm`, the
// port of provision-vm.py. It prints the human "Provisioned …" line the Python
// prints and emits no ATLAS_RESULT, because the Python declares no result: the
// controller needs only the exit code.
func provisionVM(arguments []string, output io.Writer, errorOutput io.Writer) int {
	flags := newTaskFlags("provision-vm", errorOutput)
	inputs := declareProvisionInputs(flags)
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	runner := run.NewRunner(errorOutput)
	result, err := provision.Provision(
		context.Background(), runner, inputs.params(), inputs.injectIdentity(runner),
	)
	if err != nil {
		return reportError(errorOutput, err)
	}
	if *inputs.warmSnapshotDirectory != "" && !result.WarmPairStaged {
		fmt.Fprintln(output, "Disk LV already existed and was booted; staging no warm pair (next start cold-boots).")
	}
	fmt.Fprintf(output, "Provisioned %s.\n", *inputs.virtualMachineName)
	return exitSuccess
}

// provisionInputs is provision-vm.py's ProvisionInputs as a flag table. Every
// field is one --kebab-case flag, required unless the dataclass declared a default,
// and the two list fields are repeatable — the shape `_ssh/runner.py` renders, so
// the controller does not know which implementation answers.
type provisionInputs struct {
	virtualMachineName     *string
	imageName              *string
	kernelFilename         *string
	rootfsFilename         *string
	vcpus                  *int
	memoryMB               *int
	diskGB                 *int
	sshPublicKey           *string
	macAddress             *string
	tapDevice              *string
	virtualMachineIPv6     *string
	ipv4HostCIDR           *string
	ipv4GuestCIDR          *string
	ipv4Gateway            *string
	firecrackerUID         *int
	namespace              *string
	hostVeth               *string
	namespaceVeth          *string
	cgroupArguments        *[]string
	resourceArguments      *[]string
	snapshotRootfsPath     *string
	routingBaseURL         *string
	reservedIPv4           *string
	privateAddress         *string
	tenantPrefix           *string
	dataDiskGB             *int
	dataDiskFormat         *int
	dataDiskMountAt        *string
	dataSnapshotRootfsPath *string
	preserveHostKeys       *int
	cloneRootfsDevice      *string
	warmSnapshotDirectory  *string
}

// declareProvisionInputs registers the whole table.
//
// cgroup-arg is declared as an ordinary repeatable flag rather than a required
// one, and internal/provision refuses an empty set with the reason: an unbounded
// VM is the failure this input exists to prevent, and the message that says so is
// worth more than argparse's "the following arguments are required".
//
// data-disk-format and preserve-host-keys are ints, not bools, for two reasons
// that agree: the Python's Task runner renders a bool as a truthy string, so "0"
// would read True, and Go's flag package accepts only `--flag=0` for a bool while
// the controller renders `--data-disk-format 0`.
func declareProvisionInputs(flags *taskFlags) provisionInputs {
	return provisionInputs{
		virtualMachineName:     flags.requiredText("virtual-machine-name"),
		imageName:              flags.requiredText("image-name"),
		kernelFilename:         flags.requiredText("kernel-filename"),
		rootfsFilename:         flags.requiredText("rootfs-filename"),
		vcpus:                  flags.requiredNumber("vcpus"),
		memoryMB:               flags.requiredNumber("memory-mb"),
		diskGB:                 flags.requiredNumber("disk-gb"),
		sshPublicKey:           flags.requiredText("ssh-public-key"),
		macAddress:             flags.requiredText("mac-address"),
		tapDevice:              flags.requiredText("tap-device"),
		virtualMachineIPv6:     flags.requiredText("virtual-machine-ipv6"),
		ipv4HostCIDR:           flags.requiredText("ipv4-host-cidr"),
		ipv4GuestCIDR:          flags.requiredText("ipv4-guest-cidr"),
		ipv4Gateway:            flags.requiredText("ipv4-gateway"),
		firecrackerUID:         flags.requiredNumber("atlas-fc-uid"),
		namespace:              flags.requiredText("atlas-netns"),
		hostVeth:               flags.requiredText("host-veth"),
		namespaceVeth:          flags.requiredText("namespace-veth"),
		cgroupArguments:        flags.list("cgroup-arg"),
		resourceArguments:      flags.list("resource-arg"),
		snapshotRootfsPath:     flags.text("snapshot-rootfs-path", ""),
		routingBaseURL:         flags.text("routing-base-url", ""),
		reservedIPv4:           flags.text("reserved-ipv4", ""),
		privateAddress:         flags.text("private-address", ""),
		tenantPrefix:           flags.text("tenant-prefix", ""),
		dataDiskGB:             flags.number("data-disk-gb", 0),
		dataDiskFormat:         flags.number("data-disk-format", 1),
		dataDiskMountAt:        flags.text("data-disk-mount-at", ""),
		dataSnapshotRootfsPath: flags.text("data-snapshot-rootfs-path", ""),
		preserveHostKeys:       flags.number("preserve-host-keys", 0),
		cloneRootfsDevice:      flags.text("clone-rootfs-device", ""),
		warmSnapshotDirectory:  flags.text("warm-snapshot-directory", ""),
	}
}

// params is the table as the verb's inputs. Two flags are deliberately absent:
// preserve-host-keys and data-disk-mount-at belong to the identity write, which
// provision delegates, so they reach the injector below instead of a package that
// would carry them without ever reading them.
func (inputs provisionInputs) params() provision.Params {
	return provision.Params{
		VirtualMachineName:     *inputs.virtualMachineName,
		ImageName:              *inputs.imageName,
		KernelFilename:         *inputs.kernelFilename,
		RootfsFilename:         *inputs.rootfsFilename,
		VCPUs:                  *inputs.vcpus,
		MemoryMB:               *inputs.memoryMB,
		DiskGB:                 *inputs.diskGB,
		SSHPublicKey:           *inputs.sshPublicKey,
		MACAddress:             *inputs.macAddress,
		TapDevice:              *inputs.tapDevice,
		VirtualMachineIPv6:     *inputs.virtualMachineIPv6,
		IPv4HostCIDR:           *inputs.ipv4HostCIDR,
		IPv4GuestCIDR:          *inputs.ipv4GuestCIDR,
		IPv4Gateway:            *inputs.ipv4Gateway,
		FirecrackerUID:         *inputs.firecrackerUID,
		Namespace:              *inputs.namespace,
		HostVeth:               *inputs.hostVeth,
		NamespaceVeth:          *inputs.namespaceVeth,
		CgroupArguments:        *inputs.cgroupArguments,
		ResourceArguments:      *inputs.resourceArguments,
		SnapshotRootfsPath:     *inputs.snapshotRootfsPath,
		RoutingBaseURL:         *inputs.routingBaseURL,
		ReservedIPv4:           *inputs.reservedIPv4,
		PrivateAddress:         *inputs.privateAddress,
		TenantPrefix:           *inputs.tenantPrefix,
		DataDiskGB:             *inputs.dataDiskGB,
		DataDiskFormat:         *inputs.dataDiskFormat,
		DataSnapshotRootfsPath: *inputs.dataSnapshotRootfsPath,
		CloneRootfsDevice:      *inputs.cloneRootfsDevice,
		WarmSnapshotDirectory:  *inputs.warmSnapshotDirectory,
	}
}

// injectIdentity builds the identity write provision hands the fresh disk to.
//
// The mount and the guest files live in internal/vm, so this is where the two
// halves meet: vm.InjectIdentity writes the addresses, the authorized_keys, the
// hostname, the machine-id and the data-disk fstab line, and it PRESERVES whatever
// host keys the disk carries.
//
// Preserving is right for a migration cutover — the disk moved wholesale, so its
// SSH identity must survive the move or every client's known_hosts breaks — and
// that is what preserve-host-keys asks for. It is wrong at BIRTH, which is every
// ordinary provision: the base image ships SHARED baked host keys, and a clone
// seeds from another VM's rootfs, so both must be replaced or two VMs answer with
// one identity. `boat regenerate-host-keys-vm`'s package is that replacement, run
// straight after the write.
//
// It costs a second mount of the same volume, which the Python does in one pass
// (rootfs.py's regenerate_host_keys flag). The disk ends in the same state; the
// command trace has one extra activate-mount-keygen-unmount in it.
func (inputs provisionInputs) injectIdentity(runner *run.Runner) func(context.Context, string) error {
	identity := vm.Identity{
		IPv6Address:     *inputs.virtualMachineIPv6,
		IPv4GuestCIDR:   *inputs.ipv4GuestCIDR,
		IPv4Gateway:     *inputs.ipv4Gateway,
		PrivateAddress:  *inputs.privateAddress,
		AuthorizedKeys:  *inputs.sshPublicKey,
		DataDiskMountAt: *inputs.dataDiskMountAt,
	}
	if *inputs.routingBaseURL != "" {
		identity.ExtraEnvironment = []vm.EnvironmentFile{{
			Path: routingEnvironmentPath, Content: "ATLAS_BASE_URL=" + *inputs.routingBaseURL + "\n",
		}}
	}
	uuid := *inputs.virtualMachineName
	return func(ctx context.Context, device string) error {
		if err := vm.NewManager().InjectIdentity(ctx, runner, device, uuid, identity); err != nil {
			return err
		}
		if *inputs.preserveHostKeys != 0 {
			return nil
		}
		_, err := hostkeys.RegenerateHostKeysVM(
			ctx, runner, hostkeys.RegenerateHostKeysParams{VirtualMachine: uuid},
		)
		return err
	}
}
