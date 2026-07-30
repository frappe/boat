package run

import (
	"context"
	"strings"
)

// Answer is what a probe found: the host said yes, the host said no, or the
// question could not be put to the host at all.
//
// The third value is the whole reason this type exists. Every host question Boat
// asks has three answers and only two of them are about the host — "no" is a
// fact about a VM, "could not look" is a fact about us — and a bool has nowhere
// to put the second one. Collapsing them is not a small imprecision: it is
// always "no" that a failure gets rounded to, so the collapse turns every denied
// sudo, every missing binary and every cancelled context into evidence that a
// host is emptier than it is. That is the one report a control plane must never
// be handed by accident, and it is how the same bug shipped six times through
// Runner.OK.
//
// It mirrors model.StatusUnknown, which draws this line for a VM's status for
// the same reason: Unknown means "I could not see", never "it is dead".
type Answer int

const (
	// Unknown is the zero value on purpose. A probe nobody ran, a struct field
	// nobody filled in and a switch that fell through all read as "I did not
	// look" rather than as a negative the host never gave.
	Unknown Answer = iota
	// No is a PROVEN negative: the command was allowed to run, it ran, and it
	// said no.
	No
	// Yes is a proven positive: the command ran and exited zero.
	Yes
)

func (answer Answer) String() string {
	switch answer {
	case Yes:
		return "yes"
	case No:
		return "no"
	default:
		return "could not look"
	}
}

// Probe asks the host a yes/no question and answers it in three values. The
// error is non-nil exactly when the answer is Unknown, and it says what stopped
// the probe.
//
//	answer, err := runner.Probe(ctx, "sudo test -f {}", marker)
//
// # How the three answers are told apart
//
// Exit zero is Yes. For the rest the discriminator is the command's own
// STDERR, and that is a measured rule rather than a chosen one. On a
// bootstrapped host, as the unprivileged `boat` user:
//
//	sudo test -d <dir that exists>    exit 0, stderr empty     -> Yes
//	sudo test -d <dir that does not>  exit 1, stderr empty     -> No
//	sudo test -d <no sudoers rule>    exit 1, stderr "sudo: a password is required"
//	                                                           -> Unknown
//
// A denial and a true negative share an exit code, so nothing but the presence
// of a complaint separates them. A command that could not be started at all (a
// missing binary is an exec failure here, not an exit code) and a context that
// ended are Unknown too, from invoke.
//
// # What may be asked this way, and what may not
//
// Probe is for commands whose NO IS SILENT. test(1) is the archetype and is what
// every probe in Boat is built on: it prints nothing when its answer is false,
// so anything on stderr came from something other than the answer.
//
// A command that EXPLAINS its negative on stderr cannot be probed, and the two
// that matter here were both measured saying so:
//
//	vgs --noheadings -o vg_name atlas   exit 5, `Volume group "atlas" not found`
//	nft -j list counters table inet atlas  exit 1, `Error: No such file or directory`
//
// Both are ordinary answers on a host that has not run a VM yet, both look
// exactly like a denial, and for nft not even the exit code differs. There is no
// rule that recovers the distinction, so those questions are not asked as probes
// at all: they are asked as LISTINGS — a command that exits zero and answers
// with its output, where "nothing there" is an empty listing and any non-zero
// exit is unambiguously a failure. Reach for a listing first; a probe is what is
// left when there is nothing to list.
//
// Unlike OK, this traces. A probe that could not be made is exactly the event
// worth having in the operation record, and OK's silence is why the last
// instance of this bug produced a host reporting zero VMs with nothing in the
// journal to say why.
func (runner *Runner) Probe(ctx context.Context, template string, parameters ...any) (Answer, error) {
	result, argv, err := runner.invoke(ctx, "", template, parameters)
	switch {
	case err != nil:
		return Unknown, err
	case result.exitCode == 0:
		return Yes, nil
	case strings.TrimSpace(result.standardError) == "":
		return No, nil
	}
	return Unknown, &CommandError{
		Argv:     argv,
		ExitCode: result.exitCode,
		Output:   result.standardOutput + result.standardError,
	}
}
