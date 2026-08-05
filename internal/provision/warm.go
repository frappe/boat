package provision

import (
	"context"
	"fmt"
	"strings"
)

// metadataFile is a warm clone's MMDS payload: everything the identity injection
// would have written into the disk, served to the guest over the metadata service
// instead.
//
// The hostname and machine-id rules are the identity injection's own, reproduced
// below, so a warm clone's identity is exactly what a cold provision of the same
// UUID would have given it.
type metadataFile struct {
	Identity metadataIdentity `json:"identity"`
}

type metadataIdentity struct {
	UUID         string `json:"uuid"`
	Hostname     string `json:"hostname"`
	MachineID    string `json:"machine_id"`
	IPv6         string `json:"ipv6"`
	IPv4CIDR     string `json:"ipv4_cidr"`
	IPv4Gateway  string `json:"ipv4_gateway"`
	SSHPublicKey string `json:"ssh_public_key"`
	// RoutingBaseURL is the routing base URL (spec/18); the freshen unit writes it
	// to /etc/atlas-routing.env when it adopts this clone's identity — the warm-path
	// analogue of the cold path's routing file. Empty for a VM with no routing
	// config, and the guest client then no-ops.
	RoutingBaseURL string `json:"routing_base_url"`
	// PrivateAddress is the private-plane /128 (spec/25). It is PER-CLONE, derived
	// from the clone's own UUID, so it MUST ride MMDS the way the ipv6 and ipv4 do.
	// The freshen unit writes it into /etc/atlas-network.env so the warm clone joins
	// the mesh on boot. Empty for a VM off the plane.
	PrivateAddress string `json:"private_address"`
}

// mmdsMetadata renders that payload. indent=1, one space, matching the Python.
func mmdsMetadata(params Params) (string, error) {
	return encodeJSON(metadataFile{Identity: metadataIdentity{
		UUID:           params.VirtualMachineName,
		Hostname:       hostnameFor(params.VirtualMachineName),
		MachineID:      machineIdentifierFor(params.VirtualMachineName),
		IPv6:           params.VirtualMachineIPv6,
		IPv4CIDR:       params.IPv4GuestCIDR,
		IPv4Gateway:    params.IPv4Gateway,
		SSHPublicKey:   params.SSHPublicKey,
		RoutingBaseURL: params.RoutingBaseURL,
		PrivateAddress: params.PrivateAddress,
	}}, " ")
}

// hostnameFor is the first eight characters of the UUID — enough to recognise the
// VM in a shell prompt or a journal line, short enough to read. It duplicates
// internal/vm's derivation deliberately: that one is unexported, and the two MUST
// agree, because a warm clone learns its hostname from here and a cold provision of
// the same UUID learns it from there.
func hostnameFor(uuid string) string {
	if len(uuid) > 8 {
		uuid = uuid[:8]
	}
	return "atlas-" + uuid
}

// machineIdentifierFor is the UUID's hex, which systemd's /etc/machine-id format
// wants: 32 lowercase hex characters, stable across this VM's reboots and unique
// across VMs. The same duplication, for the same reason, as hostnameFor.
func machineIdentifierFor(uuid string) string {
	hex := strings.ReplaceAll(uuid, "-", "")
	if len(hex) > 32 {
		hex = hex[:32]
	}
	return hex
}

// stageWarmPair is step 5b: hard-link the durable golden pair into this clone's
// jail and arm the marker.
//
// It runs AFTER the recursive chown, and that order is the whole point. The pair is
// HARD-LINKED from the durable artifact — N clones copy-on-write share one
// read-only memory file, same filesystem so the link always works — and a chown of
// a link chowns the SHARED inode. So the inodes stay root-owned 0644 (any per-VM
// uid can map them) and only the directory is handed to this VM's uid, for
// traversal.
//
// The marker is written LAST: it asserts a complete, matching pair, exactly the
// contract snapshot-stop writes. Restore consumes only the marker — the link
// targets are shared — and checks the staged host signature before loading.
//
// A warm clone whose disk already existed and had been booted stages nothing; its
// next start cold-boots, which the Result reports rather than leaving the operator
// to infer it from a missing marker.
func (provisioning *provisioning) stageWarmPair(ctx context.Context) error {
	if !provisioning.stageWarm {
		return nil
	}
	directory := provisioning.virtualMachine.MemorySnapshotDirectory()
	if err := provisioning.commands.InstallDirectory(ctx, directory, "0700"); err != nil {
		return err
	}
	if _, err := provisioning.commands.Run(ctx, "sudo chown {} {}", provisioning.owner(), directory); err != nil {
		return err
	}
	for _, name := range []string{"vmstate.bin", "mem.bin"} {
		source := provisioning.params.WarmSnapshotDirectory + "/" + name
		// -s, not -f: a zero-length file is a half-written golden, and restoring RAM
		// from one is a guest that never comes back.
		if !provisioning.commands.OK(ctx, "sudo test -s {}", source) {
			return fmt.Errorf("warm snapshot file missing or empty: %s; re-bake the warm golden", source)
		}
		if _, err := provisioning.commands.Run(ctx, "sudo ln -f {} {}", source, directory+"/"+name); err != nil {
			return err
		}
	}
	if _, err := provisioning.commands.Run(
		ctx, "sudo cp {} {}", provisioning.params.WarmSnapshotDirectory+"/host-signature.json",
		provisioning.virtualMachine.MemorySnapshotSignature(),
	); err != nil {
		return err
	}
	_, err := provisioning.commands.Run(ctx, "sudo touch {}", provisioning.virtualMachine.MemorySnapshotMarker())
	return err
}
