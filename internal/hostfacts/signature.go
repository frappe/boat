package hostfacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/frappe/boat/internal/model"
)

// firecrackerBinary is where Atlas's bootstrap installs Firecracker. Pinned
// rather than resolved through PATH: the signature has to name the binary the
// VMs are actually running under, and a PATH lookup could answer with a
// different build entirely.
const firecrackerBinary = "/usr/local/bin/firecracker"

// errFirecrackerAbsent marks a host that has no Firecracker at all, which is an
// un-bootstrapped machine describing itself rather than a fault.
var errFirecrackerAbsent = errors.New("no Firecracker is installed")

// signature identifies the CPU, kernel and Firecracker a memory snapshot was
// captured on. A warm snapshot only restores on a matching host — Intel↔AMD is
// unsupported, and even two same-size DigitalOcean droplets can differ, because
// the Premium tier only promises one of the latest two CPU generations and a
// live migration can move a droplet under us. So a capture records this and a
// restore refuses to load when the live host's differs, cold-booting instead,
// which is always correct.
//
// The JSON field names are the port's contract, not a detail: the captured
// signature Atlas already has on disk is compared key by key against the live
// one (scripts/vm-restore.py), so a renamed key is a snapshot that never
// restores again. Ported from scripts/lib/atlas/hostinfo.py.
type signature struct {
	CPUModel string `json:"cpu_model"`
	// Microcode is included because an update can change CPUID-visible
	// behaviour. Absent on some virtualised hosts, and recorded as "" there.
	Microcode string `json:"microcode"`
	// CPUFlagsSHA256 stands in for the CPUID surface itself. The flag set is the
	// thing a snapshot actually depends on and it is far too long to store, and
	// equality is the only comparison anyone makes of it, so it is stored hashed.
	CPUFlagsSHA256 string `json:"cpu_flags_sha256"`
	Kernel         string `json:"kernel"`
	Firecracker    string `json:"firecracker"`
}

// addSignature composes the host signature out of five things — CPU model,
// microcode, the CPUID flag digest, the kernel release, and the Firecracker
// version — and records it as the JSON document Atlas stores and a restore
// compares against.
func addSignature(ctx context.Context, commands commands, facts *model.HostFacts) error {
	cpuinfo, err := commands.Run(ctx, "cat /proc/cpuinfo")
	if err != nil {
		return fmt.Errorf("reading /proc/cpuinfo: %w", err)
	}
	firecracker, err := readFirecrackerVersion(ctx, commands)
	if err != nil {
		// A host with no Firecracker installed is one that has not been
		// bootstrapped, and it still has a CPU, a kernel and a name worth
		// reporting. Failing the whole read here left `GET /export` answering
		// 500 for every pre-bootstrap host, which is the same mistake the thin
		// pool made: observed state is meant to describe what is there, and
		// "there is no Firecracker" is a description.
		//
		// The signature is still written, with an empty Firecracker field. That
		// is deliberately NOT a signature that matches anything: a warm snapshot
		// only restores onto the CPU, kernel and Firecracker it was captured on,
		// so a host that cannot name its Firecracker must fail that comparison
		// rather than pass it by omission.
		// Only absence is tolerated. A Firecracker that is installed but cannot
		// state its version is a host whose snapshot compatibility is unknowable
		// while claiming to be able to run VMs, and that is worth failing over.
		if !errors.Is(err, errFirecrackerAbsent) {
			return err
		}
		slog.Warn("this host has no Firecracker installed", "path", firecrackerBinary, "error", err)
		firecracker = ""
	}
	signature := parseCPUSignature(cpuinfo)
	signature.Kernel, signature.Firecracker = facts.KernelVersion, firecracker
	encoded, err := json.Marshal(signature)
	if err != nil {
		return fmt.Errorf("encoding the host signature: %w", err)
	}
	facts.FirecrackerVersion, facts.HostSignature = firecracker, string(encoded)
	return nil
}

// readFirecrackerVersion asks the installed Firecracker what it is.
//
// The exit code is discarded, matching the Python: a Firecracker that prints its
// version and then exits non-zero has still answered the question. A binary that
// is not there at all is a different matter and RunUnchecked reports it, because
// a host that cannot say which Firecracker it runs cannot have its snapshots
// trusted — and an empty version is worse than an error, since two hosts that
// both fail to answer produce two signatures that match each other.
func readFirecrackerVersion(ctx context.Context, commands commands) (string, error) {
	output, err := commands.RunUnchecked(ctx, "{} --version", firecrackerBinary)
	if err != nil {
		// Wrapped so the caller can tell "not installed" from "installed and
		// unreadable". RunUnchecked only errors when the command could not be
		// started at all, which for a fixed absolute path means the binary is
		// not there.
		return "", fmt.Errorf("running %s --version: %w: %w", firecrackerBinary, errFirecrackerAbsent, err)
	}
	version := parseFirecrackerVersion(output)
	if version == "" {
		return "", errors.New(firecrackerBinary + " --version printed nothing")
	}
	return version, nil
}

// parseFirecrackerVersion takes the version token out of `firecracker
// --version` ("Firecracker v1.13.1" → "v1.13.1"), falling back to the whole
// first line on an unexpected shape — the comparison is equality, so any stable
// string works.
func parseFirecrackerVersion(output string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(output), "\n")
	for _, token := range strings.Fields(first) {
		if len(token) > 1 && token[0] == 'v' && isDigit(token[1]) {
			return token
		}
	}
	return first
}

func isDigit(character byte) bool { return character >= '0' && character <= '9' }

// parseCPUSignature reads the CPU identity out of /proc/cpuinfo's FIRST
// processor block. Every later block repeats it, so the scan stops at the blank
// line that ends the first one — on a 96-thread host that is 95 blocks of
// identical text not walked.
func parseCPUSignature(cpuinfo string) signature {
	parsed, flags := signature{}, ""
	for _, text := range strings.Split(cpuinfo, "\n") {
		if strings.TrimSpace(text) == "" {
			break
		}
		key, value, _ := strings.Cut(text, ":")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch {
		case key == "model name" && parsed.CPUModel == "":
			parsed.CPUModel = value
		case key == "microcode" && parsed.Microcode == "":
			parsed.Microcode = value
		case key == "flags" && flags == "":
			flags = value
		}
	}
	parsed.CPUFlagsSHA256 = digestFlags(flags)
	return parsed
}

// digestFlags hashes the CPUID flag set: sorted, single-space joined, SHA-256,
// first 16 hex characters. Every step of that recipe is load-bearing — it has to
// produce byte-identical output to the Python that captured the signatures
// already sitting on Atlas's hosts, or every existing warm snapshot stops
// matching the host it was taken on.
func digestFlags(flags string) string {
	fields := strings.Fields(flags)
	slices.Sort(fields)
	digest := sha256.Sum256([]byte(strings.Join(fields, " ")))
	return hex.EncodeToString(digest[:])[:16]
}
