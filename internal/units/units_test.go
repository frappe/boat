package units

import (
	"slices"
	"strings"
	"testing"
)

// The property this package exists to guarantee, stated as a test rather than
// as a comment: the per-VM units are not reachable through unit supervision.
// They have their own verbs, their own fence and their own journal, and a second
// door onto them here would be a way around all three.
func TestPerVirtualMachineUnitsAreNotSupervised(t *testing.T) {
	instances := []string{
		"firecracker-vm@06571461-3a61-4581-9e82-10d6011d0d6a.service",
		"firecracker-vm@.service",
		"firecracker-vm@atlas-pool.service",
	}
	for _, name := range instances {
		if IsSupervised(name) {
			t.Errorf("%s is supervised, and a per-VM unit must never be", name)
		}
		if _, known := commandFor(Start); !known {
			t.Fatal("start has no template, so this test proves nothing")
		}
	}
}

// The names that would turn a unit action into root. sshd is the classic — a
// control plane that can stop it locks every operator out of the host — and
// boat.service is the one a daemon must never be able to restart, because it is
// itself.
func TestTheUnitsThatWouldBeRootAreNotSupervised(t *testing.T) {
	for _, name := range []string{"sshd.service", "ssh.service", "boat.service", "", "*", "atlas-pool"} {
		if IsSupervised(name) {
			t.Errorf("%q is supervised and must not be", name)
		}
	}
}

// The allow-list is only an allow-list if every entry is a name and nothing
// else. A `*`, a space or an `@` in one would each undo it in a different way:
// a wildcard matches units nobody enumerated, a space grows the sudo command a
// second operand, and an `@` is how a per-VM instance would get in.
func TestEverySupervisedNameIsAPlainInstanceFreeUnitName(t *testing.T) {
	for _, name := range supervised {
		if !strings.HasSuffix(name, ".service") {
			t.Errorf("%q does not name a service unit", name)
		}
		if strings.ContainsAny(name, "*? \t@/") {
			t.Errorf("%q carries a character that makes it more than one name", name)
		}
	}
}

func TestSupervisedIsACopy(t *testing.T) {
	taken := Supervised()
	taken[0] = "sshd.service"

	if slices.Contains(supervised, "sshd.service") {
		t.Fatal("a caller holding the set could append to the allow-list")
	}
}

// There is no stop, and its absence is the design. A supervisor that could take
// the wake trap down would strand every sleeping VM on the host with nothing
// watching its counter, and nothing in the reconciler would ever notice.
func TestTheActionSetConvergesUpwardOnly(t *testing.T) {
	for _, refused := range []string{"stop", "kill", "disable", "mask", "reset-failed", ""} {
		if _, known := ParseAction(refused); known {
			t.Errorf("%q parsed as an action this host performs", refused)
		}
	}
	for _, accepted := range []Action{Start, Restart} {
		parsed, known := ParseAction(string(accepted))
		if !known || parsed != accepted {
			t.Errorf("%q did not parse back to itself", accepted)
		}
		if _, rendered := commandFor(accepted); !rendered {
			t.Errorf("%q has no command template", accepted)
		}
	}
}

// The templates are literals with one hole, which is what keeps internal/run's
// trust model intact: only the unit name varies, and it arrives quoted.
func TestEachActionRendersItsOwnLiteralTemplate(t *testing.T) {
	start, _ := commandFor(Start)
	restart, _ := commandFor(Restart)

	if start != "sudo systemctl start {}" || restart != "sudo systemctl restart {}" {
		t.Fatalf("got %q and %q, want one literal template each", start, restart)
	}
	if _, known := commandFor(Action("stop")); known {
		t.Error("stop rendered a template, so the verb set is wider than it claims")
	}
}
