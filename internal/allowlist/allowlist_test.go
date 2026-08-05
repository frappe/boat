package allowlist

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The repository root, from this package's directory. The check reads the same
// two artifacts an operator installs — the source tree and the allow-list file
// that ships beside it — so it is comparing the things that actually reach a
// host, not a fixture of them.
const repositoryRoot = "../.."

func sudoersPath() string { return filepath.Join(repositoryRoot, "sudoers.d", "boat") }

func sourceDirectories() []string {
	return []string{filepath.Join(repositoryRoot, "internal"), filepath.Join(repositoryRoot, "cmd")}
}

// TestEveryRenderedCommandIsGranted is the check that four of one week's
// defects would have failed on. Each was a call site added or tightened without
// the matching grant, and each passed both a green suite and a hand check on a
// host: the suite stubs above the layer that renders the string, and a hand
// check exercises the commands you thought to type.
//
// It holds the lines the daemon can reach to the allow-list, and only those: a
// verb Atlas runs over SSH is already root, so its `sudo` prefix consults
// nothing and a grant for it would be privilege nobody exercises.
func TestEveryRenderedCommandIsGranted(t *testing.T) {
	grants, err := ParseSudoers(sudoersPath())
	if err != nil {
		t.Fatalf("reading the allow-list: %v", err)
	}
	templates, _, err := DaemonReach(repositoryRoot)
	if err != nil {
		t.Fatalf("reading the call graph: %v", err)
	}
	for _, template := range templates {
		for _, command := range PrivilegedCommands(template) {
			if !anyGrantCovers(grants, command) {
				t.Errorf(
					"%s:%d renders `sudo %s`, which no grant in sudoers.d/boat authorises.\n"+
						"The daemon can reach that line, so on a host this is not a crash: sudo "+
						"denies it, and the caller reads the denial as an answer about the host.",
					template.File, template.Line, command,
				)
			}
		}
	}
}

// TestEveryGrantIsRendered fails on a grant no call site can reach. The
// allow-list's own header argues the case: a grant nothing exercises is a grant
// nobody notices is wrong, and this file has already carried one family of
// systemctl grants that were dead — one of which was worse than dead, because
// its `*` matched a space and so reached units the alias promised never to
// touch.
func TestEveryGrantIsRendered(t *testing.T) {
	grants, err := ParseSudoers(sudoersPath())
	if err != nil {
		t.Fatalf("reading the allow-list: %v", err)
	}
	// The whole tree, not just what the daemon reaches. "Is this privilege
	// still called for?" is answered by any caller at all: a grant whose call
	// site moved to a root-run verb is not dead, and reporting it as dead would
	// invite deleting a grant the daemon regains the moment a handler reaches
	// that verb again.
	templates, _, err := Templates(sourceDirectories()...)
	if err != nil {
		t.Fatalf("reading the templates: %v", err)
	}
	var commands []string
	for _, template := range templates {
		commands = append(commands, PrivilegedCommands(template)...)
	}
	for _, grant := range grants {
		if !coversAny(grant, commands) {
			t.Errorf(
				"%s grants `%s`, which nothing in the tree renders.\n"+
					"Either a call site was deleted and its privilege outlived it, or the "+
					"grant never matched the command it was written for.",
				grant.Alias, grant.Pattern,
			)
		}
	}
}

// TestEveryTemplateIsReadable guards the check above. A template assembled at
// runtime is invisible to this package, so it would pass by being unreadable
// rather than by being granted — a hole exactly where the privileged commands
// live. Keeping templates literal is also what makes `run`'s quoting model
// hold: a value belongs in a `{}`, never in the template.
//
// Scoped to what the daemon reaches, for the same reason the grant check is:
// a root-run verb has no allow-list to be checked against, so an unreadable
// template there hides nothing. `boat bootstrap` assembles one apt-get line
// from a package list and is left alone.
func TestEveryTemplateIsReadable(t *testing.T) {
	_, dynamic, err := DaemonReach(repositoryRoot)
	if err != nil {
		t.Fatalf("reading the call graph: %v", err)
	}
	for _, template := range dynamic {
		t.Errorf(
			"%s:%d builds its command template at runtime, so no check can read it. "+
				"Write the template as a literal and pass the value through a `{}` hole.",
			template.File, template.Line,
		)
	}
}

// TestTheAllowListParses runs visudo where it exists. A comment written inside
// a Cmnd_Alias breaks the backslash continuation and visudo refuses the whole
// file — which on a host means the boat user has no grants at all, and every
// verb fails at once.
func TestTheAllowListParses(t *testing.T) {
	visudo, err := exec.LookPath("visudo")
	if err != nil {
		t.Skip("visudo is not installed; the allow-list's syntax is checked on the host instead")
	}
	if output, err := exec.Command(visudo, "-cf", sudoersPath()).CombinedOutput(); err != nil {
		t.Errorf("visudo rejects sudoers.d/boat: %v\n%s", err, output)
	}
}

func anyGrantCovers(grants []Grant, command string) bool {
	for _, grant := range grants {
		if Covers(grant.Pattern, command) {
			return true
		}
	}
	return false
}

func coversAny(grant Grant, commands []string) bool {
	for _, command := range commands {
		if Covers(grant.Pattern, command) {
			return true
		}
	}
	return false
}

func TestMatchHoldsSudosRules(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		command string
		matched bool
	}{{
		name:    "a wildcard in an argument matches a space, which is why UUIDs are spelled out",
		pattern: "/usr/bin/systemctl stop firecracker-vm@*.service",
		command: "/usr/bin/systemctl stop firecracker-vm@x.service sshd.service",
		// Not a quirk to be tidied away: this exact string satisfied the wildcard
		// form of the grant on a real host, which is how stopping sshd was one
		// rendered space away. The spelled-out form below is the fix.
		matched: true,
	}, {
		name:    "the spelled-out instance cannot match a second operand",
		pattern: "/usr/bin/systemctl stop firecracker-vm@[0-9a-f][0-9a-f].service",
		command: "/usr/bin/systemctl stop firecracker-vm@ab.service sshd.service",
		matched: false,
	}, {
		name:    "a class matches exactly one character",
		pattern: "/usr/bin/systemctl stop firecracker-vm@[0-9a-f][0-9a-f].service",
		command: "/usr/bin/systemctl stop firecracker-vm@ab.service",
		matched: true,
	}, {
		name:    "a grant with no arguments does not authorise arguments",
		pattern: "/usr/bin/systemctl",
		command: "/usr/bin/systemctl stop sshd.service",
		matched: false,
	}, {
		name:    "an escaped colon is a literal, as in a link-local address",
		pattern: `/usr/sbin/ip -6 addr replace fe80\:\:a/64 dev mig6-*`,
		command: "/usr/sbin/ip -6 addr replace fe80::a/64 dev mig6-13879",
		matched: true,
	}, {
		name:    "a star in the path does not cross a directory separator",
		pattern: "/usr/bin/* stop sshd.service",
		command: "/usr/bin/sub/systemctl stop sshd.service",
		matched: false,
	}, {
		name:    "a star in an argument does cross one",
		pattern: "/usr/bin/rm -rf /var/lib/atlas/*",
		command: "/usr/bin/rm -rf /var/lib/atlas/a/b/c",
		matched: true,
	}}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Match(testCase.pattern, testCase.command); got != testCase.matched {
				t.Errorf("Match(%q, %q) = %v, want %v", testCase.pattern, testCase.command, got, testCase.matched)
			}
		})
	}
}

func TestCoversAsksWhetherAnyValueWouldBeAuthorised(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		command string
		covered bool
	}{{
		name:    "a hole can render the UUID a spelled-out class demands",
		pattern: "/usr/bin/systemctl start firecracker-vm@[0-9a-f][0-9a-f].service",
		command: "systemctl start firecracker-vm@{}.service",
		covered: true,
	}, {
		name:    "a hole cannot rescue a literal the grant does not have",
		pattern: "/usr/bin/systemctl start firecracker-vm@[0-9a-f].service",
		command: "systemctl restart firecracker-vm@{}.service",
		covered: false,
	}, {
		name:    "a different binary is never covered",
		pattern: "/usr/sbin/lvremove -f atlas/*",
		command: "lvcreate -f atlas/{}",
		covered: false,
	}, {
		name:    "a hole may render nothing at all",
		pattern: "/usr/bin/test -f /var/lib/atlas/network.env",
		command: "test -f /var/lib/atlas/{}network.env",
		covered: true,
	}, {
		name:    "wildcards on both sides still have to agree on the literals",
		pattern: "/usr/sbin/nft delete counter inet atlas wake_*",
		command: "nft delete counter inet atlas sleep_{}",
		covered: false,
	}}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Covers(testCase.pattern, testCase.command); got != testCase.covered {
				t.Errorf("Covers(%q, %q) = %v, want %v", testCase.pattern, testCase.command, got, testCase.covered)
			}
		})
	}
}
