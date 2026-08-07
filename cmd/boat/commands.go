package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/frappe/boat/internal/wire"
)

func startVirtualMachine(arguments []string, client *daemonClient, output io.Writer, errorOutput io.Writer) int {
	uuid, _, ok := uuidBeforeFlags(arguments, "start", errorOutput)
	if !ok {
		return exitUsage
	}
	var operation wire.Operation
	body := wire.StartRequest{OperationId: announce(output)}
	if err := client.post("/vms/"+uuid+"/start", body, &operation); err != nil {
		return reportError(errorOutput, err)
	}
	return reportOperation(operation, output, errorOutput)
}

func stopVirtualMachine(arguments []string, client *daemonClient, output io.Writer, errorOutput io.Writer) int {
	uuid, rest, ok := uuidBeforeFlags(arguments, "stop", errorOutput)
	if !ok {
		return exitUsage
	}
	flags := flag.NewFlagSet("boat vm stop", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	graceful := flags.Bool("graceful", true, "ask the guest to power itself off first")
	timeout := flags.Int("stop-timeout-seconds", 0, "bound the graceful drain; 0 leaves systemd's default")
	if err := flags.Parse(rest); err != nil {
		return exitUsage
	}
	body := wire.StopRequest{OperationId: announce(output), Graceful: graceful, StopTimeoutSeconds: timeout}
	var operation wire.Operation
	if err := client.post("/vms/"+uuid+"/stop", body, &operation); err != nil {
		return reportError(errorOutput, err)
	}
	return reportOperation(operation, output, errorOutput)
}

// adoptVirtualMachine asserts a desired record for a VM whose artifacts are on
// this host but which Atlas never asserted — an adopted orphan. It is the break-
// glass answer to `boat vm start`'s refusal ("assert the desired state first"):
// it issues the same PUT /vms/{uuid} Atlas issues, so the record is written
// through the one path (store.PutDesired) and the fence is set the one way.
//
// It asserts boot_epoch=1 — the initial epoch Atlas itself uses, and nothing but
// Atlas ever writes another — so the store's regression check does the safe thing
// for free: a VM that already holds a NEWER epoch has migration history (a
// retracted or evacuated source), and re-adopting it here is refused rather than
// booting a second live copy. That refusal is the point, not a rough edge.
//
// desired_power defaults to Running, which is what unblocks `boat vm start` (a
// Stopped desire is refused by the start verb on purpose); --power stopped asserts
// the durable stop the reconciler will hold. The record carries power and epoch
// and nothing else: vCPU, memory and disk are Atlas's to assert, so resize and
// rebuild still require them, and adopt is deliberately not a second source for
// numbers a shell could get wrong. Server is left empty, which asserts no
// placement — a local adopt claims authority on THIS host and lets the epoch alone
// gate the boot.
//
// `start` is NOT made to adopt-then-start on its own: silently granting boot
// authority to a VM this host holds no intent for is the split-brain the fence
// exists to prevent (internal/api/fence.go). The grant is explicit here, or it is
// Atlas's — never a side effect.
func adoptVirtualMachine(arguments []string, client *daemonClient, output io.Writer, errorOutput io.Writer) int {
	uuid, rest, ok := uuidBeforeFlags(arguments, "adopt", errorOutput)
	if !ok {
		return exitUsage
	}
	flags := flag.NewFlagSet("boat vm adopt", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	power := flags.String("power", "running", "desired power to assert: running or stopped")
	if err := flags.Parse(rest); err != nil {
		return exitUsage
	}
	desiredPower, ok := desiredPowerFrom(*power)
	if !ok {
		fmt.Fprintf(errorOutput, "boat vm adopt --power must be running or stopped, not %q\n", *power)
		return exitUsage
	}
	body := wire.DesiredVirtualMachine{Uuid: uuid, BootEpoch: 1, DesiredPower: desiredPower}
	var stored wire.DesiredVirtualMachine
	if err := client.put("/vms/"+uuid, body, &stored); err != nil {
		return reportError(errorOutput, err)
	}
	fmt.Fprintf(output, "adopted %s: desired_power=%s boot_epoch=%d — this host is now authoritative; boat vm start/stop manage it\n",
		stored.Uuid, stored.DesiredPower, stored.BootEpoch)
	return exitSuccess
}

// desiredPowerFrom maps the --power flag to the wire enum, case-insensitively, and
// reports whether it was one of the two the reconciler takes.
func desiredPowerFrom(value string) (wire.DesiredPower, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "running":
		return wire.DesiredPowerRunning, true
	case "stopped":
		return wire.DesiredPowerStopped, true
	default:
		return "", false
	}
}

// plainVerb runs one of the verbs whose entire request is its operation
// identifier: pause, resume, sleep, wake, terminate and resize.
//
// They carry nothing else because everything they would have carried is desired
// state, asserted by the PUT that precedes them, or a host fact the daemon reads
// for itself (internal/api/inputs.go). That is what lets one function serve six
// of them rather than six near-copies: the CLI has nothing to offer that the
// host does not already hold, and a flag letting an operator state a number the
// store disagrees with would be a second source of truth reachable from a shell.
//
// A resize run this way applies whatever shape Atlas last asserted, which is
// precisely what makes it useful as a break-glass command — it re-applies an
// intent the host failed to reach — and is why it takes no numbers.
func plainVerb(
	verb string, arguments []string, client *daemonClient, output io.Writer, errorOutput io.Writer,
) int {
	uuid, _, ok := uuidBeforeFlags(arguments, verb, errorOutput)
	if !ok {
		return exitUsage
	}
	var operation wire.Operation
	body := wire.OperationRequest{OperationId: announce(output)}
	if err := client.post("/vms/"+uuid+"/"+verb, body, &operation); err != nil {
		return reportError(errorOutput, err)
	}
	return reportOperation(operation, output, errorOutput)
}

// rebuildVirtualMachine is the one verb that cannot be reduced to a UUID: a
// rebuild destroys a filesystem and lays another down, and neither what to lay
// down nor what to write into it is desired state.
//
// The identity file is REQUIRED here, and refusing without it is the reason this
// is written out rather than folded into plainVerb. The wire makes identity
// optional, which is right for a caller that has written the guest's keys some
// other way; from a shell it is a trap. A rebuild carrying no identity lays down
// a pristine rootfs with no authorized_keys at all — the VM boots, every
// observable signal says the rebuild succeeded, and nobody can log in to it
// again. That is the exact bug the Atlas client had, and it is not worth
// re-introducing behind a flag an operator can forget.
//
// The file is the §7.2 blob and travels as bytes: nothing here parses it beyond
// checking it is the object the contract describes, so no guest-service field
// acquires a meaning in this command.
func rebuildVirtualMachine(arguments []string, client *daemonClient, output io.Writer, errorOutput io.Writer) int {
	uuid, rest, ok := uuidBeforeFlags(arguments, "rebuild", errorOutput)
	if !ok {
		return exitUsage
	}
	flags := flag.NewFlagSet("boat vm rebuild", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	image := flags.String("image", "", "base image to lay a pristine rootfs down from")
	snapshot := flags.String("snapshot-device", "", "device of a snapshot of this VM to restore instead")
	dataSnapshot := flags.String("data-snapshot-device", "", "device of a data-disk snapshot to restore alongside")
	identityFile := flags.String("identity-file", "", "JSON guest identity to write into the new filesystem")
	if err := flags.Parse(rest); err != nil {
		return exitUsage
	}
	body, err := rebuildBody(announce(output), *image, *snapshot, *dataSnapshot, *identityFile)
	if err != nil {
		return reportError(errorOutput, err)
	}
	var operation wire.Operation
	if err := client.post("/vms/"+uuid+"/rebuild", body, &operation); err != nil {
		return reportError(errorOutput, err)
	}
	return reportOperation(operation, output, errorOutput)
}

// rebuildBody assembles the request and refuses the two ways it can be
// meaningless: no source to lay down, and no identity to write back.
func rebuildBody(
	identifier, image, snapshot, dataSnapshot, identityFile string,
) (wire.RebuildRequest, error) {
	if image == "" && snapshot == "" {
		return wire.RebuildRequest{}, errors.New(
			"a rebuild needs a source: pass --image to lay down a pristine rootfs, or " +
				"--snapshot-device to restore one of this VM's own snapshots")
	}
	if identityFile == "" {
		return wire.RebuildRequest{}, errors.New(
			"a rebuild needs --identity-file: the new filesystem carries the SOURCE's " +
				"identity, so without one the VM boots with no authorized keys and nothing " +
				"can reach it again")
	}
	identity, err := readIdentity(identityFile)
	if err != nil {
		return wire.RebuildRequest{}, err
	}
	return wire.RebuildRequest{
		OperationId:        identifier,
		Image:              optionalText(image),
		SnapshotDevice:     optionalText(snapshot),
		DataSnapshotDevice: optionalText(dataSnapshot),
		Identity:           identity,
	}, nil
}

// readIdentity refuses a field name the contract does not have, rather than
// dropping it. An operator who misspells authorized_keys_blob would otherwise
// get exactly the unreachable VM that requiring the file at all prevents.
func readIdentity(path string) (*wire.GuestIdentity, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read the identity file %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var identity wire.GuestIdentity
	if err := decoder.Decode(&identity); err != nil {
		return nil, fmt.Errorf("could not read %s as a guest identity: %w", path, err)
	}
	return &identity, nil
}

// optionalText leaves an unset flag out of the request entirely, so the daemon
// distinguishes "no image named" from "an image named empty".
func optionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// uuidBeforeFlags takes the UUID off the front of the arguments. The documented
// grammar puts it before the flags, and Go's flag package stops parsing at the
// first non-flag word, so the two have to be separated before parsing.
func uuidBeforeFlags(arguments []string, verb string, errorOutput io.Writer) (string, []string, bool) {
	if len(arguments) == 0 || strings.HasPrefix(arguments[0], "-") {
		fmt.Fprintf(errorOutput, "boat vm %s needs a UUID before its flags\n", verb)
		return "", nil, false
	}
	return arguments[0], arguments[1:], true
}

func listVirtualMachines(client *daemonClient, output io.Writer, errorOutput io.Writer) int {
	var records []wire.VirtualMachine
	if err := client.get("/vms", &records); err != nil {
		return reportError(errorOutput, err)
	}
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "UUID\tSTATUS\tUNIT\tSNAPSHOT\tOBSERVED")
	for _, record := range records {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", record.Uuid, record.ObservedStatus,
			text(record.UnitActiveState), yesOrNo(record.HasMemorySnapshot),
			record.ObservedAt.Format(time.RFC3339))
	}
	table.Flush()
	return exitSuccess
}

func showVirtualMachine(arguments []string, client *daemonClient, output io.Writer, errorOutput io.Writer) int {
	uuid, _, ok := uuidBeforeFlags(arguments, "show", errorOutput)
	if !ok {
		return exitUsage
	}
	var record wire.VirtualMachine
	if err := client.get("/vms/"+uuid, &record); err != nil {
		return reportError(errorOutput, err)
	}
	printFields(output, [][2]string{
		{"uuid", record.Uuid},
		{"observed status", string(record.ObservedStatus)},
		{"observed at", record.ObservedAt.Format(time.RFC3339)},
		{"unit active state", text(record.UnitActiveState)},
		{"unit sub state", text(record.UnitSubState)},
		{"memory snapshot", yesOrNo(record.HasMemorySnapshot)},
		{"sleeping", yesOrNo(record.Sleeping)},
	})
	return exitSuccess
}

func showHostFacts(client *daemonClient, output io.Writer, errorOutput io.Writer) int {
	var host wire.Host
	if err := client.get("/host", &host); err != nil {
		return reportError(errorOutput, err)
	}
	printFields(output, [][2]string{
		{"hostname", host.Hostname},
		{"boat version", host.BoatVersion},
		{"started at", host.StartedAt.Format(time.RFC3339)},
		{"virtual machines", strconv.Itoa(host.VirtualMachineCount)},
	})
	return exitSuccess
}

// reportOperation prints the trace the host produced and exits non-zero when
// the operation failed, so a script driving the CLI learns the outcome without
// parsing the text.
func reportOperation(operation wire.Operation, output io.Writer, errorOutput io.Writer) int {
	fmt.Fprintf(output, "%s %s %s\n", operation.Verb, operation.Uuid, operation.Status)
	if operation.Output != nil {
		fmt.Fprint(output, *operation.Output)
	}
	reportResult(operation, output)
	if operation.Status != wire.OperationStatusFailure {
		return exitSuccess
	}
	if operation.Error != nil {
		fmt.Fprintln(errorOutput, *operation.Error)
	}
	return exitFailure
}

// reportResult prints the verb's typed result under the trace, because it is the
// one thing a verb decided that the trace does not spell out: an operator running
// `boat vm sleep` from a shell would otherwise not learn whether the guest's RAM
// was captured or the VM merely stopped. Verbs with no result print nothing.
func reportResult(operation wire.Operation, output io.Writer) {
	if operation.Result == nil {
		return
	}
	// Re-encoding what arrived as JSON cannot fail, and a result that somehow
	// would not encode is not worth failing an operation that already succeeded.
	if encoded, err := json.Marshal(*operation.Result); err == nil {
		fmt.Fprintf(output, "result %s\n", encoded)
	}
}

// announce prints the name this run is journalled under before the run starts,
// so the operator can read the record back — `/ops/<id>` — even when the
// request itself never comes back or is refused.
func announce(output io.Writer) string {
	identifier := newOperationIdentifier()
	fmt.Fprintf(output, "operation %s\n", identifier)
	return identifier
}

// newOperationIdentifier mints the name this run is journalled under. Atlas
// passes its Task name; a break-glass run has no Task, so it gets a fresh name,
// which is what keeps it from replaying somebody else's recorded result.
func newOperationIdentifier() string {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("cli-%d", time.Now().UnixNano())
	}
	return "cli-" + hex.EncodeToString(random)
}

func printFields(output io.Writer, fields [][2]string) {
	table := tabwriter.NewWriter(output, 0, 0, 2, ' ', 0)
	for _, field := range fields {
		fmt.Fprintf(table, "%s\t%s\n", field[0], field[1])
	}
	table.Flush()
}

// The wire's optional fields are absent when the daemon has nothing to say
// about them, which is not the same as saying no.
func text(value *string) string {
	if value == nil {
		return "-"
	}
	return *value
}

func yesOrNo(value *bool) string {
	if value == nil {
		return "-"
	}
	if *value {
		return "yes"
	}
	return "no"
}
