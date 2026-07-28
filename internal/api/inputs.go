// Where a verb's inputs come from, and why each one comes from there.
//
// The rule this file exists to hold in one place: anything Atlas has already
// asserted as desired state is READ FROM THE STORE rather than re-sent, and
// anything the host built for itself is read FROM THE HOST. Only what neither
// can answer — which image to reinstall from, what the fresh filesystem should
// be told about itself — travels in the request. A verb that took a per-VM
// number off the wire when the store holds one could be asked to apply a shape
// the store disagrees with, and a retry of it would apply a different shape
// again (spec/33-boat.md §2.4).

package api

import (
	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/vm"
	"github.com/frappe/boat/internal/wire"
)

// refuseStoppedDesire is §11.3: an explicit request to bring a VM back does not
// outrank an explicit assertion that it should be down.
//
// Without this the operator who stopped a VM has no way to make it stay
// stopped — the reconciler holds the line for the wake trap (internal/reconcile
// plan.go), and a verb that went around it would be a second door into the same
// room. A host holding no desired record at all is not refused: there is no
// assertion to outrank, and refusing would leave a VM nothing could ever wake.
func (server *Server) refuseStoppedDesire(uuid string, verb string) *errorResponse {
	desired, found, err := server.state.GetDesired(uuid)
	if err != nil {
		return internalFault("The desired state could not be read.", err)
	}
	if found && desired.DesiredPower == model.PowerStopped {
		return conflict("Atlas has asserted desired_power=Stopped for " + uuid +
			", so this host will not " + verb + " it; assert desired_power=Running first.")
	}
	return nil
}

// resizeRequest is the resize Atlas has already asked for, read from the store.
//
// Both refusals are the same refusal: there is nothing to apply. A zero-valued
// request would not be a smaller resize, it would write a machine config of no
// vCPU and no memory, and Firecracker reads that only at boot — so the VM would
// resize successfully now and fail to start later, with nothing connecting the
// two.
func (server *Server) resizeRequest(uuid string) (vm.ResizeRequest, *errorResponse) {
	desired, found, err := server.state.GetDesired(uuid)
	if err != nil {
		return vm.ResizeRequest{}, internalFault("The desired state could not be read.", err)
	}
	if !found {
		return vm.ResizeRequest{}, badRequest("This host holds no desired state for " + uuid +
			", and a resize applies the numbers Atlas asserted; assert them first.")
	}
	if desired.VCPUs <= 0 || desired.MemoryMegabytes <= 0 {
		return vm.ResizeRequest{}, badRequest("The desired state for " + uuid +
			" states no vCPU or no memory, and applying that would write a machine config no guest can boot.")
	}
	return resizeFrom(desired), nil
}

// resizeFrom is the desired shape as the resize verb takes it.
//
// The cgroup values are derived here rather than sent, because they are a pure
// function of the numbers Atlas already asserted and because the host is the
// only side that knows what the launcher they are spliced into looks like.
//
// DataDiskFormatted is left false, and it is the one field this cannot source
// truthfully: whether the data disk carries a filesystem is
// `data_disk_format_and_mount` on the Atlas row and it is not part of the
// desired state Boat is given. False grows the block device and not the
// filesystem on it, which is the safe direction — a volume grown without its
// filesystem is fixed by re-running this once the flag exists, while resize2fs
// pointed at a disk that is not ext4 is a conversation with a stranger's bytes.
func resizeFrom(desired model.DesiredVirtualMachine) vm.ResizeRequest {
	return vm.ResizeRequest{
		VCPUs:           desired.VCPUs,
		MemoryMB:        desired.MemoryMegabytes,
		DiskGB:          desired.DiskGigabytes,
		DataDiskGB:      desired.DataDiskGigabytes,
		CgroupArguments: vm.CgroupArguments(desired),
	}
}

// rebuildRequest joins what the caller asked for to what the store already
// holds. The sizes come from desired state; nothing else in the request has an
// answer there.
func (server *Server) rebuildRequest(uuid string, body wire.RebuildRequest) (vm.RebuildRequest, *errorResponse) {
	desired, _, err := server.state.GetDesired(uuid)
	if err != nil {
		return vm.RebuildRequest{}, internalFault("The desired state could not be read.", err)
	}
	return vm.RebuildRequest{
		Image:              optional(body.Image),
		SnapshotDevice:     optional(body.SnapshotDevice),
		DataSnapshotDevice: optional(body.DataSnapshotDevice),
		// A rebuild is not a resize: the new filesystem is grown to the size the
		// VM already has. An absent desired record leaves both zero, which grows
		// neither disk — the rebuild still lays the source down, and a resize is
		// how a VM changes size.
		DiskGB:     desired.DiskGigabytes,
		DataDiskGB: desired.DataDiskGigabytes,
		Identity:   identityFrom(body.Identity),
	}, nil
}

// identityFrom reads the guest identity without interpreting any of it. Every
// field is copied across as bytes; nothing here parses a key, validates an
// address or knows what a file called /etc/anything is for.
func identityFrom(document *wire.GuestIdentity) vm.Identity {
	if document == nil {
		return vm.Identity{}
	}
	return vm.Identity{
		IPv6Address:      optional(document.Ipv6Address),
		IPv4GuestCIDR:    optional(document.Ipv4GuestCidr),
		IPv4Gateway:      optional(document.Ipv4Gateway),
		PrivateAddress:   optional(document.PrivateAddress),
		AuthorizedKeys:   optional(document.AuthorizedKeysBlob),
		DataDiskMountAt:  optional(document.DataDiskMountAt),
		ExtraEnvironment: guestFilesFrom(document.ExtraEnv),
	}
}

func guestFilesFrom(documents *[]wire.GuestFile) []vm.EnvironmentFile {
	if documents == nil {
		return nil
	}
	files := make([]vm.EnvironmentFile, 0, len(*documents))
	for _, document := range *documents {
		files = append(files, vm.EnvironmentFile{Path: document.Path, Content: document.Content})
	}
	return files
}
