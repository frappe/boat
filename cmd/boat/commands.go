package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
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
	if operation.Status != wire.OperationStatusFailure {
		return exitSuccess
	}
	if operation.Error != nil {
		fmt.Fprintln(errorOutput, *operation.Error)
	}
	return exitFailure
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
