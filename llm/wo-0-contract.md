# WO-0 package contract — binding

Several agents build these packages at the same time. **The signatures below are
fixed.** Implement them exactly; do not rename, do not change an argument order,
do not add a required argument. Add unexported helpers freely, and add exported
symbols only when they are needed by your own package's tests.

If a signature here is genuinely wrong, say so in your report — do not silently
"fix" it, because someone else has already written a call against it.

Module: `github.com/frappe/boat`. Go 1.26. Dependency budget: `go.etcd.io/bbolt`
and the `oapi-codegen` runtime (already required). Adding any other dependency
needs an argument in the report; the stdlib is the default.

Already written and not yours to change:

- `internal/wire` — generated from `api/openapi.yaml`. Types and
  `StrictServerInterface` live here. Regenerate with `make generate`, never hand-edit.
- `internal/model` — persisted records: `VirtualMachine`, `Operation`, their
  status constants, `Operation.Finished()`, `Operation.Matches(verb, uuid)`.
- `internal/version` — `version.Version`, stamped at link time.

---

## internal/paths — port of `scripts/lib/atlas/paths.py`

Pure string derivation. No host access, no error returns. Every path in the
Python original keeps its meaning; only the naming becomes Go.

```go
package paths

const (
	AtlasRoot                = "/var/lib/atlas"
	ImagesDirectory          = AtlasRoot + "/images"
	VirtualMachinesDirectory = AtlasRoot + "/virtual-machines"
	BinDirectory             = AtlasRoot + "/bin"
	SnapshotsDirectory       = AtlasRoot + "/snapshots"

	// Jail-relative forms for Firecracker API bodies — the jailed process
	// resolves these after chroot.
	MemorySnapshotVMStateInJail = "snapshot/vmstate.bin"
	MemorySnapshotMemoryInJail  = "snapshot/mem.bin"
	MetadataInJail              = "metadata.json"

	// AF_UNIX sun_path is 108 bytes including the NUL. The jailed socket's
	// absolute path blows past it, which is why the relative-cd dance exists.
	SunPathMax = 108
)

type VirtualMachine struct{ UUID string }

func ForVirtualMachine(uuid string) VirtualMachine

func (virtualMachine VirtualMachine) Directory() string
func (virtualMachine VirtualMachine) LogDirectory() string
func (virtualMachine VirtualMachine) NetworkEnvironment() string   // network.env
func (virtualMachine VirtualMachine) FirewallEnvironment() string  // firewall.env
func (virtualMachine VirtualMachine) TunnelsDirectory() string
func (virtualMachine VirtualMachine) TunnelEnvironment(tunnelName string) string
func (virtualMachine VirtualMachine) TunnelKey(tunnelName string) string
func (virtualMachine VirtualMachine) JailChrootBase() string
func (virtualMachine VirtualMachine) JailRoot() string
func (virtualMachine VirtualMachine) RootFilesystemNode() string
func (virtualMachine VirtualMachine) DataNode() string
func (virtualMachine VirtualMachine) Kernel() string
func (virtualMachine VirtualMachine) FirecrackerConfig() string
func (virtualMachine VirtualMachine) JailerLaunch() string
func (virtualMachine VirtualMachine) MemorySnapshotDirectory() string
func (virtualMachine VirtualMachine) MemorySnapshotMarker() string
func (virtualMachine VirtualMachine) MemorySnapshotVMState() string
func (virtualMachine VirtualMachine) MemorySnapshotMemory() string
func (virtualMachine VirtualMachine) MemorySnapshotSignature() string
func (virtualMachine VirtualMachine) MetadataFile() string
func (virtualMachine VirtualMachine) APISocketDirectory() string
func (virtualMachine VirtualMachine) APISocket() string
func (virtualMachine VirtualMachine) APISocketName() string   // "firecracker.socket"
func (virtualMachine VirtualMachine) SleepingMarker() string
func (virtualMachine VirtualMachine) TrafficCounterFile() string
func (virtualMachine VirtualMachine) SystemdUnit() string      // firecracker-vm@<uuid>.service

func ImageDirectory(imageName string) string
func WarmSnapshotDirectory(snapshotName string) string
```

The venv constants (`ATLAS_VENV`, `ATLAS_PYTHON`, `ATLAS_CLI`) are **not** ported:
they describe the Python interpreter Boat exists to retire.

## internal/run — port of `scripts/lib/atlas/_run.py`

The only package that runs a subprocess. Everything else is pure functions over
strings. Read the Python module before writing this one — its docstrings carry
the reasoning (why `{}` and not `str.format`, why `install` needs a real temp
file and not `/dev/stdin`, why the Firecracker socket needs the `cd` dance).

The one deliberate change from Python: the `set -x` trace goes to a caller-supplied
writer instead of the process's stderr, because Boat folds that trace into the
operation record that Atlas shows in the Task row.

```go
package run

// CommandError is a non-zero exit, carrying enough to explain itself.
type CommandError struct {
	Argv     []string
	ExitCode int
	Output   string
}

func (commandError *CommandError) Error() string

type Runner struct{ /* unexported */ }

// NewRunner writes its command trace to trace. A nil trace discards it.
func NewRunner(trace io.Writer) *Runner

// Run renders template, runs it with no shell, and returns stdout.
// Returns *CommandError on a non-zero exit.
func (runner *Runner) Run(ctx context.Context, template string, parameters ...any) (string, error)

// RunUnchecked is Run with the exit code discarded — the Python `check=False`.
// It still returns stdout, and only returns a non-nil error if the command
// could not be started at all.
func (runner *Runner) RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)

// OK runs a command purely as a boolean gate. Never traces output, never errors.
func (runner *Runner) OK(ctx context.Context, template string, parameters ...any) bool

// Input runs a command with stdin fed to it — the `printf ... | sudo cmd` form.
func (runner *Runner) Input(ctx context.Context, stdin string, template string, parameters ...any) (string, error)

// Shell runs the rendered template through `sh -c`, so metacharacters in the
// literal template are honoured. Parameters are still quoted, so an
// interpolated value can never inject into the pipeline.
func (runner *Runner) Shell(ctx context.Context, template string, parameters ...any) (string, error)

// InstallFile writes content to destination with mode, atomically, via install(1).
func (runner *Runner) InstallFile(ctx context.Context, content string, destination string, mode string) error

// InstallDirectory creates a directory with an explicit mode.
func (runner *Runner) InstallDirectory(ctx context.Context, destination string, mode string) error

// FirecrackerAPI calls the Firecracker API over its jailed unix socket.
func (runner *Runner) FirecrackerAPI(ctx context.Context, socketDirectory, socketName, method, apiPath, body string) error

// Substitute replaces each literal `{}` in template with a shell-quoted
// parameter, leaving every other character — notably nft's `{ … }` clauses —
// untouched. Errors when the placeholder count and parameter count disagree.
func Substitute(template string, parameters ...any) (string, error)

// Render substitutes, then splits into an argv for exec with no shell.
func Render(template string, parameters ...any) ([]string, error)

// Quote returns value as exactly one POSIX shell token.
func Quote(value string) string

// Split parses a rendered command line into argv, honouring the quoting Quote emits.
func Split(line string) ([]string, error)
```

`Quote` must match `shlex.quote` and `Split` must match `shlex.split` on
everything this codebase feeds them — the pair is the conformance oracle. Test
them against the values that actually occur: paths, UUIDs, nft rule fragments
with braces and semicolons, JSON bodies, values with spaces, empty strings.

## internal/store — bbolt

Single file, transactional. Buckets: `virtual-machines`, `operations`.

```go
package store

var ErrOperationConflict = errors.New("...")

type Store struct{ /* unexported */ }

func Open(path string) (*Store, error)   // creates parent directory and buckets
func (store *Store) Close() error

func (store *Store) PutVirtualMachine(record model.VirtualMachine) error
func (store *Store) GetVirtualMachine(uuid string) (model.VirtualMachine, bool, error)
func (store *Store) ListVirtualMachines() ([]model.VirtualMachine, error)

// ClaimOperation is the idempotency primitive. In one transaction it either
// records a fresh Running operation and reports claimed=true, or finds the
// identifier already present and returns that record with claimed=false.
//
// Two concurrent posts of one identifier must not both come back claimed.
// An identifier already recorded against a different verb or VM is
// ErrOperationConflict — replay is only replay when it is the same operation.
func (store *Store) ClaimOperation(identifier, verb, uuid string) (operation model.Operation, claimed bool, err error)

// CompleteOperation writes the terminal record. Writing over an already
// terminal record must not resurrect it.
func (store *Store) CompleteOperation(operation model.Operation) error

func (store *Store) GetOperation(identifier string) (model.Operation, bool, error)
```

## internal/vm — start and stop

Ports `scripts/start-vm.py` and `scripts/stop-vm.py`, including their reasoning.
Both are addressed by UUID through the per-VM systemd unit.

```go
package vm

type Manager struct{ /* unexported */ }

func NewManager() *Manager

// Start brings up the VM's unit. Idempotent: starting a running unit is a no-op.
// Reports whether the guest was restored from a memory snapshot or cold-booted.
func (manager *Manager) Start(ctx context.Context, runner *run.Runner, uuid string) (restored bool, err error)

type StopRequest struct {
	// Graceful asks the guest to power itself off first, so it syncs before the
	// unit is stopped.
	Graceful bool
	// TimeoutSeconds bounds the graceful drain without skipping ExecStopPost.
	// Zero leaves systemd's default drain in place.
	TimeoutSeconds int
}

func (manager *Manager) Stop(ctx context.Context, runner *run.Runner, uuid string, request StopRequest) error

// Observe reads this VM's state off the host: the unit's ActiveState and
// SubState, and the on-disk markers. It never infers status from a command
// having succeeded.
func (manager *Manager) Observe(ctx context.Context, runner *run.Runner, uuid string) (model.VirtualMachine, error)

// Exists reports whether this host has a VM directory for uuid at all.
func (manager *Manager) Exists(ctx context.Context, runner *run.Runner, uuid string) bool
```

Carry over from the Python, and say why in the code:

- The failed-restore retry in `start-vm.py`: a failed restore consumes the
  marker and fails the start job while `Restart=always` schedules its own
  relaunch. On that exact signature — marker present before, gone after, start
  failed — `reset-failed` and start once more, which cold-boots.
- `is-active` after start, because `systemctl start` returns before the service
  settles.
- The graceful stop's `SendCtrlAltDel` and its 30-second poll, best-effort: a
  missing socket or a guest that ignores it falls through to the unit stop.
- The `TimeoutStopSec` drop-in under `/run` for a bounded drain, removed
  afterwards — and why it is not `systemctl kill -SIGKILL` (ExecStopPost must
  still run, or the source keeps answering NDP for a `/128`).
- `_converge_clone` after a stop: remove a leftover dm-clone so the plain LV is
  no longer held busy.

`Observe` maps to `model.VirtualMachineStatus` as: sleeping marker present →
`StatusSleeping`; unit `ActiveState=active` → `StatusRunning`; unit inactive or
failed → `StatusStopped`; the host unreadable → `StatusUnknown`.

## internal/api — the HTTP surface

Implements `wire.StrictServerInterface` over the store and the VM manager.

`NewServer` takes interfaces, not the concrete store and manager, so the
handlers can be tested against fakes without a bbolt file or a host. `cmd/boat`
passes the real `*store.Store` and `*vm.Manager`, which satisfy them.

```go
package api

// OperationStore is the slice of the store the handlers need.
type OperationStore interface {
	ClaimOperation(identifier, verb, uuid string) (model.Operation, bool, error)
	CompleteOperation(operation model.Operation) error
	GetOperation(identifier string) (model.Operation, bool, error)
	PutVirtualMachine(record model.VirtualMachine) error
	GetVirtualMachine(uuid string) (model.VirtualMachine, bool, error)
	ListVirtualMachines() ([]model.VirtualMachine, error)
}

// VirtualMachines is the slice of the VM manager the handlers need.
type VirtualMachines interface {
	Start(ctx context.Context, runner *run.Runner, uuid string) (bool, error)
	Stop(ctx context.Context, runner *run.Runner, uuid string, request vm.StopRequest) error
	Observe(ctx context.Context, runner *run.Runner, uuid string) (model.VirtualMachine, error)
	Exists(ctx context.Context, runner *run.Runner, uuid string) bool
}

type Server struct{ /* unexported */ }

func NewServer(operations OperationStore, virtualMachines VirtualMachines, startedAt time.Time) *Server

// TunnelHandler serves the tunnel listener: every request must carry the
// bearer token, compared in constant time. /health is exempt.
func (server *Server) TunnelHandler(token string) http.Handler

// SocketHandler serves /run/boat/boat.sock, where unix peer credentials are the
// authentication — the socket is 0660 and group-owned by the service user.
func (server *Server) SocketHandler() http.Handler
```

Behaviour the handlers owe:

- `start`/`stop` claim the operation first (`store.ClaimOperation`). A replay
  returns the recorded operation unchanged and runs nothing. `ErrOperationConflict`
  is a 409.
- The verb runs with a `run.Runner` whose trace is captured into
  `Operation.Output`, so the Task row reads the way it does today.
- The operation is recorded terminal — success or failure — before the response
  is written. WO-0 runs the verb inline; nothing here may outlive the request
  without a journal record behind it.
- Every response is a wire type. Errors cross the boundary as `wire.Error` with
  one plain sentence, never a stack.

## cmd/boat — the multi-call entry point

One binary, every host-side service and the operator CLI. Read `THE RULE` in
`CLAUDE.md` before shaping this.

```
boat daemon [--listen ADDR] [--socket PATH] [--store PATH] [--token-file PATH]
boat vm start <uuid>
boat vm stop <uuid> [--graceful=false] [--stop-timeout-seconds N]
boat vm ls
boat vm show <uuid>
boat host facts
boat version
```

`boat daemon` opens the store, serves both listeners, and shuts down cleanly on
SIGTERM. Every other subcommand is a **client of the same API over
`/run/boat/boat.sock`** — never a second path into the host. That is what makes the
CLI a truthful break-glass tool instead of a second implementation.

Defaults: `--listen` empty (socket only), `--socket /run/boat/boat.sock`,
`--store /var/lib/boat/boat.db`, `--token-file /etc/boat/token`.
