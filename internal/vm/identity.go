package vm

import (
	"context"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/run"
)

// Identity is what makes a freshly laid-down rootfs this VM's rootfs rather
// than the image's.
//
// It is written into the disk while it is mounted on the host, because the
// guest is not running — a rebuild happens with the VM stopped, which is the
// only moment the host may mount its root filesystem at all. Boat writes these
// values without interpreting them: an address is bytes it puts in a file, and
// what they mean is Atlas's business.
type Identity struct {
	IPv6Address string
	// IPv4GuestCIDR and IPv4Gateway are the two ends of the NAT44 /30 the guest
	// egresses through. The host side of that /30 is deliberately absent: a
	// rebuild does not touch the VM's host-side networking, so the value would
	// be accepted and ignored.
	IPv4GuestCIDR string
	IPv4Gateway   string
	// PrivateAddress is the VM's /128 on the host mesh, empty for a VM that is
	// off the private plane. The line is written either way, so the guest's
	// network unit has a defined value to test rather than a missing variable.
	PrivateAddress string
	// AuthorizedKeys is the guest's root authorized_keys file, whole. One key or
	// six, whose they are and what they may do: none of that is readable from
	// here and none of it is Boat's. It is written verbatim.
	AuthorizedKeys string
	// ExtraEnvironment is every other file the control plane wants in the fresh
	// rootfs, as {path, content} pairs Boat writes without parsing either half.
	//
	// This is the whole of the guest-service seam. A field named for what a file
	// MEANS — a routing URL, a bench setting — would put a service semantic in
	// Boat's vocabulary and make the host care what runs inside the guest; an
	// anonymous path and its bytes do not. Boat therefore cannot tell one of
	// these apart from another, which is the property being bought.
	ExtraEnvironment []EnvironmentFile
	// DataDiskMountAt re-establishes the data disk's fstab line in the fresh
	// rootfs. Empty means no data mount, and no line is written.
	DataDiskMountAt string
}

// EnvironmentFile is one guest file and its content, both opaque to Boat.
type EnvironmentFile struct {
	Path    string
	Content string
}

const (
	hostKeyPath       = "/etc/ssh/ssh_host_ed25519_key"
	dataDiskLabelLine = "LABEL=" + dataFilesystemTag
)

// InjectIdentity writes a VM's identity through an already-selected device — the
// exported seam a cross-host migration's InjectingIdentity phase drives.
//
// Rebuild owns device selection (it just laid the volume down) and calls the
// unexported form directly; a migration selects the device itself — the live
// dm-clone the guest will boot on, or the plain LV once the clone is gone — and
// hands it here. Host keys are PRESERVED either way (ensureHostKeys), because the
// disk moved wholesale and its SSH identity must survive the move; that is the
// regenerate_host_keys=False contract the migration script named.
func (manager *Manager) InjectIdentity(
	ctx context.Context, runner *run.Runner, devicePath string, uuid string, identity Identity,
) error {
	return manager.injectIdentity(ctx, manager.commandsFor(runner), devicePath, uuid, identity)
}

// injectIdentity mounts the disk and writes this VM's identity into it.
//
// The mount is torn down on every path out, success or failure. A mount left
// behind holds the volume open, and the next thing a rebuild does to that
// volume is hand it to a jailed Firecracker.
func (manager *Manager) injectIdentity(
	ctx context.Context, commands commands, devicePath string, uuid string, identity Identity,
) error {
	output, err := commands.Run(ctx, "sudo mktemp -d /tmp/atlas-mount-XXXXXX")
	if err != nil {
		return err
	}
	mountPoint := strings.TrimSpace(output)
	if _, err := commands.Run(ctx, "sudo mount {} {}", devicePath, mountPoint); err != nil {
		return err
	}
	defer manager.unmount(ctx, commands, mountPoint)
	return manager.writeIdentity(ctx, commands, mountPoint, uuid, identity)
}

// unmount is unchecked from end to end: it is cleanup, and reporting a failure
// here would fail a rebuild whose work succeeded. The rmdir is expected to fail
// whenever the umount did.
func (manager *Manager) unmount(ctx context.Context, commands commands, mountPoint string) {
	commands.RunUnchecked(ctx, "sudo umount {}", mountPoint)
	commands.RunUnchecked(ctx, "sudo rmdir {}", mountPoint)
}

func (manager *Manager) writeIdentity(
	ctx context.Context, commands commands, mountPoint string, uuid string, identity Identity,
) error {
	if err := manager.writeAuthorizedKeys(ctx, commands, mountPoint, identity.AuthorizedKeys); err != nil {
		return err
	}
	if err := manager.writeNetworkEnvironment(ctx, commands, mountPoint, identity); err != nil {
		return err
	}
	if err := manager.writeHostname(ctx, commands, mountPoint, hostnameFor(uuid)); err != nil {
		return err
	}
	if err := manager.ensureHostKeys(ctx, commands, mountPoint, hostnameFor(uuid)); err != nil {
		return err
	}
	if err := commands.InstallFile(
		ctx, machineIdentifierFor(uuid)+"\n", mountPoint+"/etc/machine-id", "0444",
	); err != nil {
		return err
	}
	if err := manager.writeExtraEnvironment(ctx, commands, mountPoint, identity.ExtraEnvironment); err != nil {
		return err
	}
	if identity.DataDiskMountAt == "" {
		return nil
	}
	return manager.writeDataDiskMount(ctx, commands, mountPoint, identity.DataDiskMountAt)
}

func (manager *Manager) writeAuthorizedKeys(
	ctx context.Context, commands commands, mountPoint string, authorizedKeys string,
) error {
	if err := commands.InstallDirectory(ctx, mountPoint+"/root/.ssh", "0700"); err != nil {
		return err
	}
	return commands.InstallFile(
		ctx, authorizedKeys+"\n", mountPoint+"/root/.ssh/authorized_keys", "0600",
	)
}

// writeExtraEnvironment lays down the files Boat was handed but does not read.
//
// The path is checked because it is joined onto the host's mount point: an
// absolute path with no `..` in it lands inside the filesystem being rebuilt,
// and anything else lands somewhere on the HOST, as root. Refused rather than
// sanitised — a caller that meant a guest path and wrote something else has a
// bug worth hearing about, and quietly rewriting it would hide it.
//
// The containing directory has to exist in the rootfs already; `install` does
// not create one, and a rebuild that had to mkdir -p its way to a guest config
// file is a rebuild writing somewhere the image never had.
func (manager *Manager) writeExtraEnvironment(
	ctx context.Context, commands commands, mountPoint string, files []EnvironmentFile,
) error {
	for _, file := range files {
		if !strings.HasPrefix(file.Path, "/") || strings.Contains(file.Path, "..") {
			return fmt.Errorf(
				"guest file %q must be an absolute path with no '..' in it, or it would not stay inside the rebuilt filesystem",
				file.Path,
			)
		}
		if err := commands.InstallFile(ctx, file.Content, mountPoint+file.Path, "0644"); err != nil {
			return err
		}
	}
	return nil
}

func (manager *Manager) writeNetworkEnvironment(
	ctx context.Context, commands commands, mountPoint string, identity Identity,
) error {
	content := fmt.Sprintf(
		"VIRTUAL_MACHINE_IPV6=%s\nVIRTUAL_MACHINE_IPV4=%s\nVIRTUAL_MACHINE_IPV4_GATEWAY=%s\nPRIVATE_ADDRESS=%s\n",
		identity.IPv6Address, identity.IPv4GuestCIDR, identity.IPv4Gateway, identity.PrivateAddress,
	)
	return commands.InstallFile(ctx, content, mountPoint+"/etc/atlas-network.env", "0644")
}

// writeHostname writes /etc/hostname and appends the 127.0.1.1 line that
// `hostname -f` resolves against — the Debian convention. The append goes
// through `tee -a` fed on stdin, so the hostname is data on a pipe and never a
// word on a command line, and its echo is redirected away so it cannot land in
// the operation's parsed output.
func (manager *Manager) writeHostname(
	ctx context.Context, commands commands, mountPoint string, hostname string,
) error {
	if err := commands.InstallFile(ctx, hostname+"\n", mountPoint+"/etc/hostname", "0644"); err != nil {
		return err
	}
	return appendToFile(ctx, commands, mountPoint+"/etc/hosts", fmt.Sprintf("\n127.0.1.1\t%s\n", hostname))
}

// ensureHostKeys PRESERVES whatever host keys the disk carries.
//
// They are the VM's SSH identity: a restore carries the VM's own keys in the
// snapshot, a rebuild-from-image carries the image's. Changing them here would
// break every client's known_hosts on every rebuild — which looks exactly like
// an attack — so rotation is its own explicit action and is not a side effect
// of this one. The single exception is a disk with NO keys at all, because a
// keyless sshd does not start, and that is a self-heal rather than a rotation.
func (manager *Manager) ensureHostKeys(
	ctx context.Context, commands commands, mountPoint string, hostname string,
) error {
	if commands.OK(ctx, "sudo test -f {}", mountPoint+hostKeyPath) {
		return nil
	}
	if err := commands.InstallDirectory(ctx, mountPoint+"/etc/ssh", "0755"); err != nil {
		return err
	}
	// Inherited rsa/ecdsa keys go even though only ed25519 is generated: a
	// snapshot source may carry them, and keeping them would let the new VM
	// answer with the source VM's identity.
	for _, algorithm := range []string{"rsa", "ecdsa"} {
		stale := fmt.Sprintf("%s/etc/ssh/ssh_host_%s_key", mountPoint, algorithm)
		if _, err := commands.Run(ctx, "sudo rm -f {} {}", stale, stale+".pub"); err != nil {
			return err
		}
	}
	key := mountPoint + hostKeyPath
	if _, err := commands.Run(ctx, "sudo rm -f {} {}", key, key+".pub"); err != nil {
		return err
	}
	// ed25519 only: ~0.03s to generate against ~0.9s for RSA, and every modern
	// client negotiates it first.
	_, err := commands.Run(
		ctx, "sudo ssh-keygen -q -t ed25519 -f {} -N {} -C {}", key, "", "root@"+hostname,
	)
	return err
}

// writeDataDiskMount re-establishes the guest's data-disk mount.
//
// Keyed by LABEL rather than by UUID because the rebuild rerolls the
// filesystem's UUID and keeps its label; `nofail` so a missing or unformatted
// data disk cannot hold up the guest's boot. Idempotent: a restored rootfs may
// already carry the line, and a duplicate fstab entry is its own failure.
func (manager *Manager) writeDataDiskMount(
	ctx context.Context, commands commands, mountPoint string, mountAt string,
) error {
	// The same guard writeExtraEnvironment makes on a guest path, and for the
	// same reason: mountAt comes from the rebuild request body, and it is joined
	// onto the host's mount point and handed to `mkdir -p`. An absolute path with
	// no `..` stays inside the rebuilt filesystem; anything else walks out onto
	// the host, where mkdir -p would create a root-owned directory. Refused rather
	// than sanitised — a caller that meant a guest path and sent something else
	// has a bug worth hearing about.
	if !strings.HasPrefix(mountAt, "/") || strings.Contains(mountAt, "..") {
		return fmt.Errorf(
			"data-disk mount point %q must be an absolute path with no '..' in it, or it would not stay inside the rebuilt filesystem",
			mountAt,
		)
	}
	fstab := mountPoint + "/etc/fstab"
	if commands.OK(ctx, "sudo grep -q {} {}", dataDiskLabelLine, fstab) {
		return nil
	}
	if _, err := commands.Run(ctx, "sudo mkdir -p {}", mountPoint+mountAt); err != nil {
		return err
	}
	line := fmt.Sprintf("%s\t%s\text4\tdefaults,nofail\t0\t2\n", dataDiskLabelLine, mountAt)
	return appendToFile(ctx, commands, fstab, line)
}

// appendToFile appends content to a root-owned file inside the mounted rootfs.
// The redirection makes this a shell line, and the path is substituted into it
// with the same quoting every other command gets, so only the literal template
// is ever shell.
func appendToFile(ctx context.Context, commands commands, path string, content string) error {
	rendered, err := run.Substitute("tee -a {} >/dev/null", path)
	if err != nil {
		return err
	}
	_, err = commands.Input(ctx, content, "sudo sh -c {}", rendered)
	return err
}

// hostnameFor is the first eight characters of the UUID — enough to recognise
// the VM in a shell prompt or a journal line, short enough to read.
func hostnameFor(uuid string) string {
	if len(uuid) > 8 {
		uuid = uuid[:8]
	}
	return "atlas-" + uuid
}

// machineIdentifierFor is the UUID's hex, which systemd's /etc/machine-id
// format wants: 32 lowercase hex characters, stable across this VM's reboots
// and unique across VMs.
func machineIdentifierFor(uuid string) string {
	hex := strings.ReplaceAll(uuid, "-", "")
	if len(hex) > 32 {
		hex = hex[:32]
	}
	return hex
}
