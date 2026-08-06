package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
)

// A Task verb is a host operation Atlas drives over SSH: `boat snapshot-vm
// --virtual-machine-name <uuid> --snapshot-rootfs-path /dev/atlas/…`. It is the
// same contract the Python verbs answer, because the controller side does not
// change when the implementation does — `_ssh/runner.py` renders the flags from
// a variables dict and `TaskResult.parse` reads one line back off stdout.
//
// Two halves, and both are load-bearing:
//
//   - Every field of the Python's TaskInputs is a `--kebab-case` flag, required
//     unless it declared a default. argparse exits 2 naming the missing flag;
//     so does this, because a Task that fails for the wrong reason costs an
//     operator the round trip that would have told them which flag they forgot.
//   - Every field of its TaskResult goes out as ONE `ATLAS_RESULT=<json>` line.
//     Atlas parses it with `cls(**payload)`, so a key that is missing, extra or
//     misspelled is a TypeError on the controller rather than a wrong value —
//     loud, but only once it has already run on the host.
//
// These functions are the verb's ONE implementation, reached two ways. Over the
// CLI they run directly, as `boat snapshot-vm …`, which is how an operator drives
// a host and how Atlas still drives the verbs whose grants the boat user does not
// yet hold. Over the daemon they run in-process behind POST /v1/host-verbs/{verb}
// (see cmd/boat/hostverb_dispatch.go and internal/api/hostverbs.go), journaled by
// op_id like every lifecycle verb — which is how Atlas drives the verbs the boat
// user IS granted, over HTTP rather than SSH (spec/33 §2.4). Same function, so the
// CLI cannot drift from the endpoint; the seam is which caller reaches it.

// resultMarker is scripts/lib/atlas/_task.py's RESULT_MARKER. It is distinct
// from any command the verb runs, so trace noise never collides with it.
const resultMarker = "ATLAS_RESULT="

// emitResult writes the one machine-readable line the controller parses back.
//
// A map rather than a struct with tags: the keys are the Python dataclass's
// field names, and writing them beside their values is what lets a reader check
// the contract without opening two files. Order does not matter — the far side
// is a dict.
func emitResult(output io.Writer, fields map[string]any) error {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "%s%s\n", resultMarker, encoded)
	return err
}

// taskFlags is one verb's argument list: the Python TaskInputs rendered as
// flags, with the required ones remembered so a missing flag is named.
type taskFlags struct {
	set      *flag.FlagSet
	required []string
}

func newTaskFlags(verb string, errorOutput io.Writer) *taskFlags {
	set := flag.NewFlagSet(verb, flag.ContinueOnError)
	set.SetOutput(errorOutput)
	return &taskFlags{set: set}
}

// text declares an optional string flag, defaulting the way the dataclass field
// does. The controller drops empty values from the command line, so an optional
// flag that never appears is the normal case rather than an error.
func (flags *taskFlags) text(name string, fallback string) *string {
	return flags.set.String(name, fallback, "")
}

func (flags *taskFlags) requiredText(name string) *string {
	flags.required = append(flags.required, name)
	return flags.set.String(name, "", "")
}

func (flags *taskFlags) number(name string, fallback int) *int {
	return flags.set.Int(name, fallback, "")
}

func (flags *taskFlags) requiredNumber(name string) *int {
	flags.required = append(flags.required, name)
	return flags.set.Int(name, 0, "")
}

// list declares a repeatable flag: `--public-allow-port 80 --public-allow-port
// 443` collects both, which is how the controller renders a list field.
func (flags *taskFlags) list(name string) *[]string {
	values := &repeatable{}
	flags.set.Var(values, name, "")
	return &values.values
}

// parse reads the arguments and reports the first required flag left out.
func (flags *taskFlags) parse(arguments []string) error {
	if err := flags.set.Parse(arguments); err != nil {
		return err
	}
	supplied := map[string]bool{}
	flags.set.Visit(func(supply *flag.Flag) { supplied[supply.Name] = true })
	for _, name := range flags.required {
		if !supplied[name] {
			return fmt.Errorf("--%s is required", name)
		}
	}
	if remaining := flags.set.Args(); len(remaining) > 0 {
		return fmt.Errorf("unexpected argument %q", remaining[0])
	}
	return nil
}

// repeatable is a flag that may be given more than once.
type repeatable struct{ values []string }

func (values *repeatable) String() string { return strings.Join(values.values, ",") }

func (values *repeatable) Set(value string) error {
	values.values = append(values.values, value)
	return nil
}
