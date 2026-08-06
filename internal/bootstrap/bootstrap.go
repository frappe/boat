// Package bootstrap brings a bare Ubuntu host to VM-ready with one command —
// `boat bootstrap`. Everything a host needs to run Firecracker microVMs, done by
// boat itself rather than an SSH-driven Python script: the packages, Firecracker
// and the jailer, the kernel sysctls and modules, the LVM thin pool VM disks are
// carved from, the nft scaffold every VM's networking re-asserts, and the on-disk
// directory tree.
//
// It ports scripts/bootstrap-server.py's host-side work. It is idempotent and
// re-runnable (every step guards on what is already there), the same contract the
// Python has. It runs the privileged commands through the same run.Runner every
// other boat verb uses, so a bootstrap is dogfooding: no host command is typed by
// hand, all of it is boat driving the host.
//
// What is deliberately NOT here yet: the CIS hardening drop-ins (sshd, module
// blocklist, unattended-upgrades) — hardening, not VM-capability — and Atlas
// registration. Those are additive and noted in llm/wo-3b-notes.md.
package bootstrap

import (
	"context"
	"fmt"
	"strings"

	"github.com/frappe/boat/internal/run"
)

const (
	// DefaultFirecrackerVersion is the release boat installs when the caller names
	// none, matching the Firecracker the staging fleet already runs. Both
	// firecracker and jailer ship in one tarball. It is exported because it is also
	// the CLI flag's default, and a default written down twice is a default that
	// drifts.
	DefaultFirecrackerVersion = "v1.16.0"

	// The LVM thin pool VM disks are thin CoW snapshots of. On a stock cloud droplet
	// the only disk is the mounted root, so the pool is backed by a sparse loopback
	// file (the DO-droplet path); a bare-metal box with a spare disk would back it
	// with the real device — not ported here, the staging fleet is droplets.
	volumeGroup   = "atlas"
	poolName      = "pool0"
	poolDirectory = "/var/lib/atlas/pool"
	poolImage     = poolDirectory + "/atlas-pool.img"
	poolDataSize  = "200G"
	poolMetaSize  = "1G"

	// Where the release tarball's two binaries land. Named once: the install
	// writes them and the result reads their versions back, and those two agreeing
	// is what makes the reported version a fact about this host rather than about
	// whatever else is on PATH.
	firecrackerBinary = "/usr/local/bin/firecracker"
	jailerBinary      = "/usr/local/bin/jailer"
)

// packages are the apt dependencies: LVM + nftables for the host plane, curl/tar
// for the Firecracker download, e2fsprogs/squashfs for images, wireguard-tools for
// the mesh, and the migration/backup helpers so an Active host is migration-capable
// without a re-bootstrap. Verbatim from bootstrap-server.py PACKAGES.
var packages = []string{
	"ca-certificates", "curl", "e2fsprogs", "iproute2", "jq", "lvm2", "nftables",
	"squashfs-tools", "thin-provisioning-tools", "wireguard-tools",
	"qemu-utils", "nbd-client", "socat", "zstd",
}

// additiveModules are loaded best-effort and persisted: the WireGuard mesh carrier
// and the migration clone/nbd targets. nbd/dm_clone live in linux-modules-extra
// (not the base cloud kernel), so they load only after that package installs, and
// a host that never migrates does not need them to boot a VM. dm_thin_pool (the
// pool target) is required and loaded separately, fail-loud.
var additiveModules = []string{"wireguard", "nbd", "dm_clone"}

// Params is what the Task carries. Both fields are the Python BootstrapInputs
// dataclass's, so `boat bootstrap` answers the same argument surface
// bootstrap-server.py did and Atlas renders one command line either way.
//
// Both are optional, and empty means "what this host already implies" rather
// than "nothing": the controller drops empty values from the command line, and an
// operator running `boat bootstrap` by hand on a bare box should not have to
// know its own architecture to bring it up.
type Params struct {
	// FirecrackerVersion is the release to install; empty takes
	// DefaultFirecrackerVersion.
	FirecrackerVersion string
	// Architecture is the `uname -m` the caller believes this host to be; empty
	// takes whatever the host says it is. A non-empty value that disagrees with
	// the host is refused, not corrected — see checkArchitecture.
	Architecture string
}

// Host brings this host to VM-ready and reports what it left behind. Progress
// goes to the runner's trace (stderr), so `boat bootstrap` reads like the
// Python's `+ command` log; the Result goes to the caller, which prints it as the
// one ATLAS_RESULT= line Atlas parses.
//
// It is deliberately not called `Run`, and that is a fact about internal/
// allowlist rather than about taste. That package resolves an unqualified
// `x.Run()` — which is every `runner.Run` in the module — to any function of that
// name when the calling package has none of its own, so a package-level `Run`
// here makes the daemon appear to call the host bootstrap. It would then demand
// sudoers grants for `apt-get`, `modprobe` and `rm -rf /tmp/...`, standing
// privileges for the unprivileged `boat` user that nothing running as `boat` ever
// exercises — for a verb that only ever runs as root, over SSH or under an
// operator's hand.
func Host(ctx context.Context, runner *run.Runner, params Params) (Result, error) {
	architecture, err := hostArchitecture(ctx, runner, params.Architecture)
	if err != nil {
		return Result{}, err
	}
	version := params.FirecrackerVersion
	if version == "" {
		version = DefaultFirecrackerVersion
	}
	steps := []struct {
		name string
		fn   func(context.Context, *run.Runner) error
	}{
		{"kvm", ensureKVM},
		{"packages", installPackages},
		{"firecracker", func(ctx context.Context, runner *run.Runner) error {
			return installFirecracker(ctx, runner, version, architecture)
		}},
		{"sysctls", installSysctls},
		{"modules", loadModules},
		{"directories", makeDirectories},
		{"host-controls", hostControls},
		{"thin-pool", EnsureThinPool},
		{"nft-scaffold", ensureScaffold},
	}
	for _, step := range steps {
		if err := step.fn(ctx, runner); err != nil {
			return Result{}, fmt.Errorf("bootstrap %s: %w", step.name, err)
		}
	}
	return readResult(ctx, runner, architecture)
}

// hostArchitecture reads what this host is and holds the caller's claim to it.
func hostArchitecture(ctx context.Context, runner *run.Runner, claimed string) (string, error) {
	actual, err := runner.Run(ctx, "uname -m")
	if err != nil {
		return "", err
	}
	actual = strings.TrimSpace(actual)
	return actual, checkArchitecture(actual, claimed)
}

// checkArchitecture refuses a bootstrap whose caller and host disagree, the same
// first thing bootstrap-server.py did.
//
// It matters because the architecture is not only a check: it selects the
// Firecracker tarball. Installing the aarch64 build on an x86_64 host leaves a
// binary that cannot exec, and the failure surfaces at the first VM start rather
// than here, on the host that could still be fixed.
func checkArchitecture(actual string, claimed string) error {
	if claimed == "" || claimed == actual {
		return nil
	}
	return fmt.Errorf("architecture mismatch: host is %s, expected %s", actual, claimed)
}

// ensureKVM refuses a host that cannot run Firecracker at all.
func ensureKVM(ctx context.Context, runner *run.Runner) error {
	if !runner.OK(ctx, "test -w /dev/kvm") {
		return fmt.Errorf("/dev/kvm is not available; the host must support hardware virtualization")
	}
	return nil
}

// installPackages waits for the first-boot apt to finish, then installs the
// dependency set. The cloud-init wait is what stops a fresh droplet's bootstrap
// racing the vendor's own apt and failing on a held lock.
func installPackages(ctx context.Context, runner *run.Runner) error {
	runner.RunUnchecked(ctx, "sudo cloud-init status --wait")
	if _, err := runner.Run(ctx, "sudo apt-get -o DPkg::Lock::Timeout=300 update"); err != nil {
		return err
	}
	arguments := append([]string{"sudo apt-get -o DPkg::Lock::Timeout=300 install -y"}, packages...)
	_, err := runner.Run(ctx, strings.Join(arguments, " "))
	return err
}

// installFirecracker downloads and installs firecracker + jailer, gated on either
// binary being absent or the wrong version.
func installFirecracker(ctx context.Context, runner *run.Runner, version string, architecture string) error {
	if firecrackerCurrent(ctx, runner, firecrackerBinary, version) && firecrackerCurrent(ctx, runner, jailerBinary, version) {
		return nil
	}
	download := firecrackerDownload(version, architecture)
	runner.RunUnchecked(ctx, "sudo rm -rf /tmp/firecracker-install")
	if _, err := runner.Run(ctx, "sudo mkdir -p /tmp/firecracker-install"); err != nil {
		return err
	}
	// The whole fetch runs under one `sudo sh -c` so the cd, the curl and the tar
	// share a privileged shell — the url is quoted into the inner script, which is
	// then one argv token to sh, so it can never break out of its slot.
	fetch, err := run.Substitute("cd /tmp/firecracker-install && curl -fsSL {} | tar -xz", download.url)
	if err != nil {
		return err
	}
	if _, err := runner.Run(ctx, "sudo sh -c {}", fetch); err != nil {
		return err
	}
	for destination, source := range download.binaries {
		if _, err := runner.Run(ctx, "sudo install -m 0755 {} {}", source, destination); err != nil {
			return err
		}
	}
	runner.RunUnchecked(ctx, "sudo rm -rf /tmp/firecracker-install")
	return nil
}

// download is where one Firecracker release lives and what it unpacks to: the
// tarball's url, and each installed path mapped to the file inside the archive it
// comes from.
type download struct {
	url      string
	binaries map[string]string
}

// firecrackerDownload derives that release layout from the version and the
// architecture. Pure, because it is the one part of the install that a test can
// hold against bootstrap-server.py's `_install_firecracker` without a host — and
// a wrong url here is a bootstrap that fails minutes in, on the network.
func firecrackerDownload(version string, architecture string) download {
	release := "release-" + version + "-" + architecture
	unpacked := func(binary string) string {
		return fmt.Sprintf("/tmp/firecracker-install/%s/%s-%s-%s", release, binary, version, architecture)
	}
	return download{
		url: fmt.Sprintf(
			"https://github.com/firecracker-microvm/firecracker/releases/download/%s/firecracker-%s-%s.tgz",
			version, version, architecture,
		),
		binaries: map[string]string{
			firecrackerBinary: unpacked("firecracker"),
			jailerBinary:      unpacked("jailer"),
		},
	}
}

// firecrackerCurrent reports whether the binary at path is the wanted version.
func firecrackerCurrent(ctx context.Context, runner *run.Runner, path string, version string) bool {
	output, err := runner.RunUnchecked(ctx, "{} --version", path)
	if err != nil {
		return false
	}
	// `firecracker --version` prints "Firecracker v1.16.0" on the first line.
	return strings.Contains(strings.SplitN(output, "\n", 2)[0], version)
}

// installSysctls writes the forwarding + hardening drop-in and applies it. The
// v6/v4 forwarding and proxy_ndp lines are load-bearing for the routed-tap model.
func installSysctls(ctx context.Context, runner *run.Runner) error {
	if err := runner.InstallFile(ctx, sysctlConf, "/etc/sysctl.d/60-atlas.conf", "0644"); err != nil {
		return err
	}
	_, err := runner.Run(ctx, "sudo sysctl --system")
	return err
}

// loadModules loads the pool target (required) and the mesh/migration targets
// (best-effort, after their package), and persists them for reboots.
func loadModules(ctx context.Context, runner *run.Runner) error {
	// nbd/dm_clone ship in linux-modules-extra, version-pinned to the running kernel
	// — never the floating -generic metapackage, which can drag in a different one.
	kernel, err := runner.Run(ctx, "uname -r")
	if err != nil {
		return err
	}
	runner.RunUnchecked(ctx, "sudo apt-get -o DPkg::Lock::Timeout=300 install -y linux-modules-extra-{}", strings.TrimSpace(kernel))

	if _, err := runner.Run(ctx, "sudo modprobe dm_thin_pool"); err != nil {
		return err
	}
	for _, module := range additiveModules {
		runner.RunUnchecked(ctx, "sudo modprobe {}", module)
	}
	content := "dm_thin_pool\n" + strings.Join(additiveModules, "\n") + "\n"
	return runner.InstallFile(ctx, content, "/etc/modules-load.d/60-atlas.conf", "0644")
}

// makeDirectories lays the on-disk tree every VM and image lives under.
func makeDirectories(ctx context.Context, runner *run.Runner) error {
	for path, mode := range map[string]string{
		"/var/lib/atlas":                  "0700",
		"/var/lib/atlas/images":           "0700",
		"/var/lib/atlas/virtual-machines": "0700",
		"/var/lib/atlas/run":              "0700",
	} {
		if err := runner.InstallDirectory(ctx, path, mode); err != nil {
			return err
		}
	}
	return nil
}

// hostControls turns off the cross-VM memory side channels: KSM page dedup and
// swap (guest RAM must never hit disk). Both idempotent and best-effort.
func hostControls(ctx context.Context, runner *run.Runner) error {
	if runner.OK(ctx, "test -w /sys/kernel/mm/ksm/run") {
		runner.Input(ctx, "0\n", "sudo tee /sys/kernel/mm/ksm/run")
	}
	runner.RunUnchecked(ctx, "sudo swapoff -a")
	return nil
}
