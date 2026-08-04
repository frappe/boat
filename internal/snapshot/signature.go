package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
)

// firecrackerBinary is where Atlas's bootstrap installs Firecracker — pinned, not
// resolved through PATH, because the signature must name the binary the VMs
// actually run under. Same path internal/hostfacts uses.
const firecrackerBinary = "/usr/local/bin/firecracker"

// hostSignature identifies the CPU, kernel and Firecracker a warm memory snapshot
// was captured on. A memory snapshot only restores on a matching host — Intel↔AMD
// is unsupported, and even two same-size DigitalOcean droplets can differ — so a
// warm capture records this and vm-restore refuses to load when the live host's
// differs, cold-booting instead, which is always correct.
//
// The JSON field names are the port's contract: vm-restore compares the captured
// document key by key against the live one, so a renamed key is a snapshot that
// never restores. Ported from scripts/lib/atlas/hostinfo.py. internal/hostfacts
// also ports hostinfo.py, but bundled into a full capacity read; this focused
// version keeps a warm capture's command sequence to three reads and controls the
// on-disk file format (indent=1) the Python wrote.
type hostSignature struct {
	CPUModel       string `json:"cpu_model"`
	Microcode      string `json:"microcode"`
	CPUFlagsSHA256 string `json:"cpu_flags_sha256"`
	Kernel         string `json:"kernel"`
	Firecracker    string `json:"firecracker"`
}

// readHostSignature composes the live host's signature: CPU identity from
// /proc/cpuinfo, the kernel release, and the Firecracker version. Compared by
// plain equality against a captured one, so any stable string works.
func readHostSignature(ctx context.Context, cmd commands) (hostSignature, error) {
	cpuinfo, err := cmd.Run(ctx, "cat /proc/cpuinfo")
	if err != nil {
		return hostSignature{}, err
	}
	kernel, err := cmd.Run(ctx, "uname -r")
	if err != nil {
		return hostSignature{}, err
	}
	// RunUnchecked, matching hostfacts: a Firecracker that prints its version and
	// then exits non-zero has still answered. RunUnchecked errors only when the
	// binary is not there at all, and a host that cannot name its Firecracker
	// records an empty version — deliberately NOT a signature that matches anything.
	version, err := cmd.RunUnchecked(ctx, "{} --version", firecrackerBinary)
	if err != nil {
		return hostSignature{}, err
	}
	signature := parseCPUSignature(cpuinfo)
	signature.Kernel = strings.TrimSpace(kernel)
	signature.Firecracker = parseFirecrackerVersion(version)
	return signature, nil
}

// parseCPUSignature reads the CPU identity out of /proc/cpuinfo's FIRST processor
// block; every later block repeats it, so the scan stops at the blank line that
// ends the first one. Ports hostinfo.py parse_cpu_signature.
func parseCPUSignature(cpuinfo string) hostSignature {
	parsed, flags := hostSignature{}, ""
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
// first 16 hex characters. Every step is load-bearing — it must produce
// byte-identical output to the Python that captured the signatures already on
// Atlas's hosts, or every existing warm snapshot stops matching its host.
func digestFlags(flags string) string {
	fields := strings.Fields(flags)
	slices.Sort(fields)
	digest := sha256.Sum256([]byte(strings.Join(fields, " ")))
	return hex.EncodeToString(digest[:])[:16]
}

// parseFirecrackerVersion takes the version token out of `firecracker --version`
// ("Firecracker v1.16.0" → "v1.16.0"), falling back to the whole first line on an
// unexpected shape. Ports hostinfo.py parse_firecracker_version.
func parseFirecrackerVersion(output string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(output), "\n")
	for _, token := range strings.Fields(first) {
		if len(token) > 1 && token[0] == 'v' && token[1] >= '0' && token[1] <= '9' {
			return token
		}
	}
	return first
}

// signatureFileContent is the host-signature.json body a warm capture stages
// beside the memory pair: pretty-printed with a trailing newline, matching the
// Python's json.dumps(signature, indent=1) + "\n". The exact bytes do not affect
// correctness (vm-restore parses it back), but reproducing the Python keeps the
// port differential.
func signatureFileContent(signature hostSignature) (string, error) {
	encoded, err := json.MarshalIndent(signature, "", " ")
	if err != nil {
		return "", err
	}
	return string(encoded) + "\n", nil
}

// signatureResultJSON is the compact JSON the result carries back, matching the
// Python's json.dumps(signature) (no indent).
func signatureResultJSON(signature hostSignature) (string, error) {
	encoded, err := json.Marshal(signature)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
