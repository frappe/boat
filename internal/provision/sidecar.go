package provision

import (
	"fmt"
	"strings"
)

// networkEnvironment renders the network.env sidecar the vm-network-up hook reads
// back, byte-shape identical to the shell heredoc it replaced.
//
// It is what makes a VM's networking stable across host reboots: the tap, the
// address, the per-VM namespace and veth names and the uid are all here, so the
// host reconstructs the whole plane after a reboot without consulting the Frappe
// database. internal/sidecar is the reader of this format; provision is its writer.
func networkEnvironment(params Params) string {
	var environment strings.Builder
	fmt.Fprintf(&environment, "TAP_DEVICE=%s\n", params.TapDevice)
	fmt.Fprintf(&environment, "VIRTUAL_MACHINE_IPV6=%s\n", params.VirtualMachineIPv6)
	fmt.Fprintf(&environment, "ATLAS_NETNS=%s\n", params.Namespace)
	fmt.Fprintf(&environment, "HOST_VETH=%s\n", params.HostVeth)
	fmt.Fprintf(&environment, "NAMESPACE_VETH=%s\n", params.NamespaceVeth)
	fmt.Fprintf(&environment, "IPV4_HOST_CIDR=%s\n", params.IPv4HostCIDR)
	fmt.Fprintf(&environment, "IPV4_GUEST_CIDR=%s\n", params.IPv4GuestCIDR)
	fmt.Fprintf(&environment, "ATLAS_FC_UID=%d\n", params.FirecrackerUID)
	// Written only when a Reserved IP is attached. vm-network-up reads it with a
	// default and skips the 1:1-NAT block when it is absent, so an ordinary VM's env
	// is unchanged.
	if params.ReservedIPv4 != "" {
		fmt.Fprintf(&environment, "RESERVED_IPV4=%s\n", params.ReservedIPv4)
	}
	// The private-plane identity (spec/25). Written TOGETHER — vm-network-up gates
	// the private block on both — and only when the VM has a tenant. Absent on a
	// pre-feature or tenant-less VM, so the private block is a complete no-op there.
	if params.PrivateAddress != "" && params.TenantPrefix != "" {
		fmt.Fprintf(&environment, "PRIVATE_ADDRESS=%s\n", params.PrivateAddress)
		fmt.Fprintf(&environment, "TENANT_PREFIX=%s\n", params.TenantPrefix)
	}
	return environment.String()
}
