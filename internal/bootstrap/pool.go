package bootstrap

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/frappe/boat/internal/run"
)

// ensureThinPool creates (or re-asserts) the LVM thin pool VM disks are carved
// from, backed by a sparse loopback file — the stock-cloud-droplet path, where the
// only disk is the mounted root. Idempotent: an existing pool is re-activated, its
// loop device re-bound, and nothing re-created.
//
// Ported from lvm.py ThinPool.ensure + PoolBacking (loopback branch). The
// double existence check guards a reboot race the Python documents: pvcreate/
// vgcreate/lvcreate each only run when their object is absent.
func ensureThinPool(ctx context.Context, runner *run.Runner) error {
	if runner.OK(ctx, "sudo lvs --noheadings {}/{}", volumeGroup, poolName) {
		if _, err := ensureLoopDevice(ctx, runner); err != nil {
			return err
		}
		runner.RunUnchecked(ctx, "sudo vgchange -ay -K {}", volumeGroup)
		return nil
	}

	loop, err := ensureLoopDevice(ctx, runner)
	if err != nil {
		return err
	}
	// Register the loop device in LVM's durable devices file so pvcreate/vgcreate
	// accept it across reboots. Best-effort: older LVM predates the devices file.
	runner.RunUnchecked(ctx, "sudo lvmdevices --adddev {}", loop)
	if !runner.OK(ctx, "sudo pvs {}", loop) {
		if _, err := runner.Run(ctx, "sudo pvcreate --yes {}", loop); err != nil {
			return err
		}
	}
	if !runner.OK(ctx, "sudo vgs {}", volumeGroup) {
		if _, err := runner.Run(ctx, "sudo vgcreate {} {}", volumeGroup, loop); err != nil {
			return err
		}
	}
	if !runner.OK(ctx, "sudo lvs --noheadings {}/{}", volumeGroup, poolName) {
		if _, err := runner.Run(ctx,
			"sudo lvcreate --type thin-pool --name {} --poolmetadatasize {} --extents 100%FREE {}",
			poolName, poolMetaSize, volumeGroup,
		); err != nil {
			return err
		}
	}
	runner.RunUnchecked(ctx, "sudo vgchange -ay -K {}", volumeGroup)
	return nil
}

// ensureLoopDevice creates the sparse backing file if absent and binds it to a
// loop device, returning the device path. A file already bound reuses its device.
func ensureLoopDevice(ctx context.Context, runner *run.Runner) (string, error) {
	if err := runner.InstallDirectory(ctx, poolDirectory, "0700"); err != nil {
		return "", err
	}
	if !runner.OK(ctx, "test -f {}", poolImage) {
		if _, err := runner.Run(ctx, "sudo truncate -s {} {}", poolDataSize, poolImage); err != nil {
			return "", err
		}
	}
	bound, err := runner.RunUnchecked(ctx, "sudo losetup -j {}", poolImage)
	if err != nil {
		return "", err
	}
	if device, _, found := strings.Cut(strings.TrimSpace(bound), ":"); found && device != "" {
		return device, nil
	}
	device, err := runner.Run(ctx, "sudo losetup --find --show {}", poolImage)
	return strings.TrimSpace(device), err
}

// ensureScaffold lays the host-wide nft floor every VM's networking re-asserts:
// the table, the forward chain, the shared park dummy sleeping VMs route out, the
// guest-IMDS drop, the private-plane default-deny, and the NAT44 egress masquerade.
// Idempotent — each object is guarded on presence. This is the same scaffold
// vmnetwork.scaffold rebuilds on the first VM after a reboot; bootstrap lays it
// down so the floor exists before any VM.
func ensureScaffold(ctx context.Context, runner *run.Runner) error {
	if !runner.OK(ctx, "sudo nft list table inet atlas") {
		if _, err := runner.Run(ctx, "sudo nft add table inet atlas"); err != nil {
			return err
		}
	}
	if !runner.OK(ctx, "sudo nft list chain inet atlas forward") {
		if _, err := runner.Run(ctx, "sudo nft add chain inet atlas forward {}", forwardChainSpecification); err != nil {
			return err
		}
	}
	if !runner.OK(ctx, "ip link show atlas-park0") {
		if _, err := runner.Run(ctx, "sudo ip link add atlas-park0 type dummy"); err != nil {
			return err
		}
	}
	if _, err := runner.Run(ctx, "sudo ip link set atlas-park0 up"); err != nil {
		return err
	}
	forward, err := runner.Run(ctx, "sudo nft list chain inet atlas forward")
	if err != nil {
		return err
	}
	if !strings.Contains(forward, "ip daddr 169.254.169.254") {
		if _, err := runner.Run(ctx, "sudo nft add rule inet atlas forward ip daddr 169.254.169.254 drop"); err != nil {
			return err
		}
	}
	if !strings.Contains(forward, "ip6 daddr fdaa::/16 drop") {
		if _, err := runner.Run(ctx, "sudo nft add rule inet atlas forward ip6 daddr fdaa::/16 drop"); err != nil {
			return err
		}
	}
	uplink, err := defaultRouteDevice(ctx, runner)
	if err != nil {
		return err
	}
	if !runner.OK(ctx, "sudo nft list chain inet atlas postrouting") {
		if _, err := runner.Run(ctx, "sudo nft add chain inet atlas postrouting {}", postroutingChainSpecification); err != nil {
			return err
		}
	}
	postrouting, err := runner.Run(ctx, "sudo nft list chain inet atlas postrouting")
	if err != nil {
		return err
	}
	if !strings.Contains(postrouting, "ip saddr 100.64.0.0/16") {
		if _, err := runner.Run(ctx, "sudo nft add rule inet atlas postrouting ip saddr 100.64.0.0/16 oifname {} masquerade", uplink); err != nil {
			return err
		}
	}
	return nil
}

// defaultRouteDevice is the interface carrying the v4 default route — where the
// masquerade sends egress. Read-only.
func defaultRouteDevice(ctx context.Context, runner *run.Runner) (string, error) {
	output, err := runner.Run(ctx, "ip -j route show default")
	if err != nil {
		return "", err
	}
	var routes []struct {
		Device string `json:"dev"`
	}
	if err := json.Unmarshal([]byte(output), &routes); err != nil || len(routes) == 0 || routes[0].Device == "" {
		return "", err
	}
	return routes[0].Device, nil
}

const (
	forwardChainSpecification     = "{ type filter hook forward priority filter; policy accept; }"
	postroutingChainSpecification = "{ type nat hook postrouting priority srcnat; policy accept; }"
)
