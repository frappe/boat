package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestEveryTaskVerbTakesThePythonsFlags pins the argument surface Atlas renders.
//
// The controller does not know which implementation answers: `_ssh/runner.py`
// turns a variables dict into `--kebab-case` flags and runs `<verb> --flags` on
// the host. So a flag this side spells differently is not a compile error and
// not a test failure — it is a Task that fails on a host, once, after everything
// before it has already run. The names below are each Python TaskInputs field,
// and they are the reason this table is worth its length.
func TestEveryTaskVerbTakesThePythonsFlags(t *testing.T) {
	verbs := map[string][]string{
		// scripts/provision-vm.py — the whole ProvisionInputs dataclass, and the
		// longest argument surface Atlas renders. The DERIVED block (mac-address
		// through namespace-veth) is required for a reason worth restating here: each
		// is a controller-allocated value, and a defaulted one mis-wires the jail or
		// the network rather than failing.
		"provision-vm": {
			"virtual-machine-name", "image-name", "kernel-filename", "rootfs-filename",
			"vcpus", "memory-mb", "disk-gb", "ssh-public-key",
			"mac-address", "tap-device", "virtual-machine-ipv6",
			"ipv4-host-cidr", "ipv4-guest-cidr", "ipv4-gateway",
			"atlas-fc-uid", "atlas-netns", "host-veth", "namespace-veth",
			"cgroup-arg", "resource-arg", "snapshot-rootfs-path", "routing-base-url",
			"reserved-ipv4", "private-address", "tenant-prefix",
			"data-disk-gb", "data-disk-format", "data-disk-mount-at",
			"data-snapshot-rootfs-path", "preserve-host-keys",
			"clone-rootfs-device", "warm-snapshot-directory",
		},
		// scripts/snapshot-vm.py
		"snapshot-vm": {"virtual-machine-name", "snapshot-rootfs-path", "data-snapshot-rootfs-path"},
		// scripts/snapshot-stop-vm.py
		"snapshot-stop-vm": {"virtual-machine-name", "atlas-fc-uid"},
		// scripts/warm-snapshot-vm.py
		"warm-snapshot-vm": {
			"virtual-machine-name", "atlas-fc-uid", "snapshot-rootfs-path", "memory-directory",
		},
		// scripts/delete-snapshot-vm.py
		"delete-snapshot-vm": {"snapshot-rootfs-path", "data-snapshot-rootfs-path", "memory-directory"},
		// scripts/upload-snapshot-s3.py
		"upload-snapshot-s3": {"snapshot-name", "objects-json"},
		// scripts/restore-snapshot-s3.py
		"restore-snapshot-s3": {"snapshot-name", "objects-json"},
		// scripts/sync-image.py
		"sync-image": {
			"image-name", "kernel-url", "kernel-filename", "kernel-sha256",
			"rootfs-url", "rootfs-filename", "rootfs-sha256", "default-disk-gb", "guest-network-unit",
		},
		// scripts/promote-snapshot-image.py
		"promote-snapshot-image": {
			"snapshot-rootfs-path", "image-name", "disk-gigabytes",
			"rootfs-filename", "source-image", "kernel-filename",
		},
		// scripts/regenerate-host-keys-vm.py
		"regenerate-host-keys-vm": {"virtual-machine-name"},
		// scripts/issue-cert.py
		"issue-cert": {"domain", "acme-directory-url", "account-email", "dns-authenticator", "certbot-arg"},
		// scripts/mgmt-firewall-apply.py and its two siblings
		"mgmt-firewall-apply":   {"wg-port", "public-interface", "revert-seconds", "public-allow-port"},
		"mgmt-firewall-confirm": {"wg-port", "public-interface", "public-allow-port"},
		"mgmt-firewall-revert":  {},
		// scripts/reset-server.py takes no inputs at all
		"reset-server": {},
		// scripts/bootstrap-server.py — the BootstrapInputs dataclass. Both flags
		// DEFAULT here where the Python required them, because `boat bootstrap` is
		// also the by-hand command that brings a bare host up, and an operator
		// standing on the box should not have to name its own architecture to use
		// it. Atlas sends both regardless (SCRIPT_FORMS["bootstrap-server"]), so
		// the Task's command line is unchanged by the defaults.
		"bootstrap": {"firecracker-version", "architecture"},
		// scripts/firewall-apply.py. --rule is repeatable and absent on a
		// deny-all-public firewall, which is a configuration rather than an omission.
		"firewall-apply": {"virtual-machine-name", "action", "rule"},
		// scripts/vm-tunnel.py — everything below --action is up-only, and a down is
		// dispatched with none of it.
		"vm-tunnel": {
			"tunnel-name", "virtual-machine-name", "interface", "action",
			"listen-port", "client-public-key", "client-address", "host-address",
		},
		// scripts/poll-vm-traffic.py and scripts/probe-woken-vms.py. Both take the
		// whole VM list as ONE json argument, and the two documents differ: the poll
		// takes objects carrying an address, the probe takes bare UUIDs.
		"poll-vm-traffic": {"vms-json"},
		"probe-woken-vms": {"vms-json"},
		// scripts/export-cleanup-source.py
		"export-cleanup-source": {"image-name", "nbd-port"},
	}

	for verb, flags := range verbs {
		t.Run(verb, func(t *testing.T) {
			var output, errorOutput bytes.Buffer
			// -h asks the flag set to describe itself and stops before the verb
			// reaches the host, which is what makes this runnable with no host.
			dispatch([]string{verb, "-h"}, &output, &errorOutput)
			described := errorOutput.String()
			for _, flag := range flags {
				if !strings.Contains(described, "-"+flag) {
					t.Errorf("`boat %s` does not take --%s:\n%s", verb, flag, described)
				}
			}
		})
	}
}

// TestHelpExitsZero. Atlas proves a verb is dispatchable on a host by running
// `boat <verb> --help` and reading the exit code (atlas/atlas/reset.py), so a
// help that exited non-zero would report every correctly installed host as
// broken — and the check exists precisely to be believed before a wipe.
func TestHelpExitsZero(t *testing.T) {
	for _, verb := range []string{"reset-server", "snapshot-vm", "mgmt-firewall-revert"} {
		var output, errorOutput bytes.Buffer
		if code := dispatch([]string{verb, "--help"}, &output, &errorOutput); code != exitSuccess {
			t.Errorf("`boat %s --help` exited %d, want 0", verb, code)
		}
	}
}

// TestATaskVerbNamesTheFlagItWasNotGiven. argparse exits 2 naming the missing
// flag; a verb that instead ran with an empty value would do half its work on a
// host before anyone learned which flag was forgotten.
func TestATaskVerbNamesTheFlagItWasNotGiven(t *testing.T) {
	var output, errorOutput bytes.Buffer

	code := dispatch([]string{"snapshot-vm", "--virtual-machine-name", "abc"}, &output, &errorOutput)

	if code == exitSuccess {
		t.Fatal("a snapshot with no --snapshot-rootfs-path succeeded")
	}
	if !strings.Contains(errorOutput.String(), "--snapshot-rootfs-path is required") {
		t.Errorf("the failure does not name the missing flag: %q", errorOutput.String())
	}
}

// TestAnUnknownFlagIsRefused. A flag the verb does not take is the controller
// and the host disagreeing about the contract; running anyway would apply an
// argument that silently went nowhere.
func TestAnUnknownFlagIsRefused(t *testing.T) {
	var output, errorOutput bytes.Buffer

	code := dispatch([]string{"reset-server", "--everything"}, &output, &errorOutput)

	if code == exitSuccess {
		t.Fatal("an unknown flag was accepted")
	}
}

// TestTheResultLineIsWhatAtlasParses. `TaskResult.parse` reads the LAST line
// carrying the marker and calls `cls(**payload)`, so the marker text, the
// one-line shape and the exact keys are all contract.
func TestTheResultLineIsWhatAtlasParses(t *testing.T) {
	var output bytes.Buffer

	if err := emitResult(&output, map[string]any{"size_bytes": 42, "data_size_bytes": 0}); err != nil {
		t.Fatalf("emitResult: %v", err)
	}

	line := strings.TrimSuffix(output.String(), "\n")
	if strings.Contains(line, "\n") {
		t.Fatalf("the result spans more than one line: %q", output.String())
	}
	payload, found := strings.CutPrefix(line, "ATLAS_RESULT=")
	if !found {
		t.Fatalf("no ATLAS_RESULT= marker in %q", line)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		t.Fatalf("the payload is not JSON: %v", err)
	}
	if fields["size_bytes"] != float64(42) {
		t.Errorf("size_bytes came back as %v", fields["size_bytes"])
	}
	if _, ok := fields["data_size_bytes"]; !ok {
		t.Error("data_size_bytes is missing; the controller's dataclass would raise on it")
	}
}

// TestARepeatedFlagCollects mirrors argparse's `action="append"`, which is how a
// list field crosses the boundary: `--public-allow-port 80 --public-allow-port
// 443`. The alternative the shell needed — one newline-joined string — is
// exactly what the typed argv replaced.
func TestARepeatedFlagCollects(t *testing.T) {
	var errorOutput bytes.Buffer
	flags := newTaskFlags("mgmt-firewall-apply", &errorOutput)
	ports := flags.list("public-allow-port")

	if err := flags.parse([]string{"--public-allow-port", "80", "--public-allow-port", "443"}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(*ports) != 2 || (*ports)[0] != "80" || (*ports)[1] != "443" {
		t.Errorf("got %v, want [80 443]", *ports)
	}
}

// TestAnAbsentListIsAnEmptyListNotNull. The controller's field is a list, and
// `json.Marshal` writes a nil slice as `null` — which reaches the dataclass as
// None and fails on the first iteration rather than at the boundary.
func TestAnAbsentListIsAnEmptyListNotNull(t *testing.T) {
	var output bytes.Buffer

	if err := emitResult(&output, map[string]any{"public_allow_ports": ports(nil)}); err != nil {
		t.Fatalf("emitResult: %v", err)
	}

	if !strings.Contains(output.String(), `"public_allow_ports":[]`) {
		t.Errorf("an absent list did not render as []: %s", output.String())
	}
}
