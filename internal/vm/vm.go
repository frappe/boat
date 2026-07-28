// Package vm owns the mechanics of a VM's life on this host: bringing its
// systemd unit up and down, and reading back what actually happened.
//
// Every VM is one systemd instance, firecracker-vm@<uuid>.service, and every
// verb here is a sequence of commands against that unit. The unit — not this
// package — owns the interesting work: ExecStartPre re-activates the disk and
// builds the netns/veth/tap, ExecStartPost restores a memory snapshot when one
// is staged, ExecStopPost tears the networking back down. The code here exists
// to drive that unit and to survive the two ways it fails on a real host:
//
//   - a restore that fails, which consumes its marker and fails the start job
//     while Restart=always quietly relaunches the VM behind the controller;
//   - a stop that skips ExecStopPost, which leaves the host answering proxy-NDP
//     for a /128 it no longer owns and collides with the next migration.
//
// Ported from scripts/start-vm.py, stop-vm.py, pause-vm.py, resume-vm.py,
// sleep-vm.py, wake-vm.py, resize-vm.py, rebuild-vm.py and terminate-vm.py,
// comments and all. Two verbs go around the unit rather than through it, and
// both say so where they do: pause and resume speak to the Firecracker API
// (which is why a paused VM keeps its RAM), and resize, rebuild and terminate
// reach past it to the VM's LVM volumes.
//
// Observe is new, and is the reason Boat exists: Atlas used to set a VM's
// status from whether its Task succeeded, which recorded the controller's
// intentions rather than the host's state. Observe reads the host.
package vm

import (
	"context"
	"time"

	"github.com/frappe/boat/internal/fcattach"
	"github.com/frappe/boat/internal/park"
	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/run"
)

// commands is everything this package does to the host, and the only seam it
// has. It exists because the command sequence a verb emits is the whole of what
// a machine with no Firecracker and no systemd can check — and that sequence is
// exactly what a differential test against the Python compares. Outside tests
// there is one implementation, *run.Runner, and there is never a second.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
	// Input feeds a command's stdin. It is here for the two appends a rebuild
	// makes into a mounted rootfs (`tee -a` on /etc/hosts and /etc/fstab), where
	// the content is data and must not become part of a command line.
	Input(ctx context.Context, stdin string, template string, parameters ...any) (string, error)
	InstallFile(ctx context.Context, content string, destination string, mode string) error
	InstallDirectory(ctx context.Context, destination string, mode string) error
	FirecrackerAPI(ctx context.Context, socketDirectory, socketName, method, apiPath, body string) error
}

var _ commands = (*run.Runner)(nil)

// virtualMachineFiles is the slice of the path layout this package addresses a
// VM through. Naming it here keeps the dependency on internal/paths to one
// function, so a test can state the layout it expects as literal strings
// instead of reaching into another package's derivation.
type virtualMachineFiles struct {
	unit                    string
	directory               string
	networkEnvironment      string
	jailRoot                string
	jailerLaunch            string
	firecrackerConfig       string
	rootFilesystemNode      string
	dataNode                string
	memorySnapshotDirectory string
	memorySnapshotMarker    string
	memorySnapshotVMState   string
	memorySnapshotMemory    string
	sleepingMarker          string
	apiSocket               string
	apiSocketDirectory      string
	apiSocketName           string
}

func filesFor(uuid string) virtualMachineFiles {
	virtualMachine := paths.ForVirtualMachine(uuid)
	return virtualMachineFiles{
		unit:                    virtualMachine.SystemdUnit(),
		directory:               virtualMachine.Directory(),
		networkEnvironment:      virtualMachine.NetworkEnvironment(),
		jailRoot:                virtualMachine.JailRoot(),
		jailerLaunch:            virtualMachine.JailerLaunch(),
		firecrackerConfig:       virtualMachine.FirecrackerConfig(),
		rootFilesystemNode:      virtualMachine.RootFilesystemNode(),
		dataNode:                virtualMachine.DataNode(),
		memorySnapshotDirectory: virtualMachine.MemorySnapshotDirectory(),
		memorySnapshotMarker:    virtualMachine.MemorySnapshotMarker(),
		memorySnapshotVMState:   virtualMachine.MemorySnapshotVMState(),
		memorySnapshotMemory:    virtualMachine.MemorySnapshotMemory(),
		sleepingMarker:          virtualMachine.SleepingMarker(),
		apiSocket:               virtualMachine.APISocket(),
		apiSocketDirectory:      virtualMachine.APISocketDirectory(),
		apiSocketName:           virtualMachine.APISocketName(),
	}
}

// clock is the seam over real time. The graceful stop polls for half a minute,
// and a test that spent half a minute on it would not get run.
type clock interface {
	Now() time.Time
	Sleep(duration time.Duration)
}

type systemClock struct{}

func (systemClock) Now() time.Time               { return time.Now() }
func (systemClock) Sleep(duration time.Duration) { time.Sleep(duration) }

// Manager performs VM operations on this host. It holds no per-VM state: a VM's
// state lives on the host, and Observe is how this package learns it.
type Manager struct {
	commandsFor func(runner *run.Runner) commands
	filesFor    func(uuid string) virtualMachineFiles
	clock       clock
	// park arms the SYN trap that makes a sleeping VM reachable again. It is a
	// field for the same reason the others are: it runs real ip and nft commands,
	// and a test that could not substitute it would have to touch the host.
	park func(ctx context.Context, runner *run.Runner, uuid string) error
	// liveness confirms that a Firecracker is answering on a VM's API socket and
	// reports the guest state it named. A field for the same reason park is: it
	// speaks HTTP to a jailed process through sudo, and a test that could not
	// substitute it would have to run a Firecracker.
	liveness func(ctx context.Context, runner *run.Runner, uuid string) (fcattach.Process, bool, error)
	// wakeTrapResident reports whether this process is running the wake reflex a
	// sleeping VM's return depends on. See requireWakeTrap for why a sleep is
	// gated on it, and internal/park.Resident for why the fact lives there.
	wakeTrapResident func() bool
}

// NewManager returns a Manager wired to the real host.
func NewManager() *Manager {
	return &Manager{
		commandsFor:      func(runner *run.Runner) commands { return runner },
		filesFor:         filesFor,
		clock:            systemClock{},
		park:             park.ParkVirtualMachine,
		liveness:         fcattach.Find,
		wakeTrapResident: park.Resident,
	}
}

// Exists reports whether this host has a VM directory for uuid at all. It is a
// question about the host's disk, so it asks the host: the answer distinguishes
// "not mine" from "mine and broken", and those get very different handling one
// layer up.
func (manager *Manager) Exists(ctx context.Context, runner *run.Runner, uuid string) bool {
	files := manager.filesFor(uuid)
	return manager.commandsFor(runner).OK(ctx, "sudo test -d {}", files.directory)
}

// memorySnapshotMarkerPresent asks the host rather than stat-ing the path in
// process: the marker lives inside the jail, under root-owned 0700 directories,
// so a stat would report "absent" for a marker that is plainly there.
func (manager *Manager) memorySnapshotMarkerPresent(
	ctx context.Context, commands commands, files virtualMachineFiles,
) bool {
	return commands.OK(ctx, "sudo test -f {}", files.memorySnapshotMarker)
}
