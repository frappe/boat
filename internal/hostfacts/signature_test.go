package hostfacts

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestParseCPUSignatureReadsOnlyTheFirstProcessorBlock(t *testing.T) {
	parsed := parseCPUSignature(cpuinfoOutput)

	if parsed.CPUModel != "Intel(R) Xeon(R) Gold 6140 CPU @ 2.30GHz" {
		t.Errorf("CPUModel = %q, want the first block's model name", parsed.CPUModel)
	}
	if parsed.Microcode != "0x1" {
		t.Errorf("Microcode = %q, want 0x1", parsed.Microcode)
	}
	if parsed.CPUFlagsSHA256 != flagsDigest {
		t.Errorf("CPUFlagsSHA256 = %q, want %q — the digest the Python computes for these flags",
			parsed.CPUFlagsSHA256, flagsDigest)
	}
}

// Some virtualised hosts print no microcode line. That is recorded as "" and is
// not an error: it is a real property of the host, and it compares equal only
// against another host that also lacks it.
func TestParseCPUSignatureToleratesAHostWithNoMicrocode(t *testing.T) {
	cpuinfo := "processor\t: 0\nmodel name\t: AMD EPYC 7763 64-Core Processor\nflags\t\t: fpu vme de\n\n"

	parsed := parseCPUSignature(cpuinfo)

	if parsed.Microcode != "" {
		t.Errorf("Microcode = %q, want empty", parsed.Microcode)
	}
	if parsed.CPUModel != "AMD EPYC 7763 64-Core Processor" || parsed.CPUFlagsSHA256 == "" {
		t.Errorf("parsed = %+v, want the model and a flags digest", parsed)
	}
}

// A cpuinfo this parser recognises nothing in still digests to the sha256 of the
// empty string rather than to nothing at all, which is what makes the digest
// field comparable on every host.
func TestParseCPUSignatureOnACpuinfoWithNoFlags(t *testing.T) {
	parsed := parseCPUSignature("processor\t: 0\n")

	if parsed.CPUFlagsSHA256 != emptyFlagsDigest {
		t.Errorf("CPUFlagsSHA256 = %q, want %q", parsed.CPUFlagsSHA256, emptyFlagsDigest)
	}
}

// Sorting is part of the recipe: the kernel's flag order is not guaranteed
// stable across boots, and an unsorted digest would make a host stop matching
// its own snapshots.
func TestFlagDigestIgnoresTheOrderTheKernelPrintedFlagsIn(t *testing.T) {
	if digestFlags("sse2 avx fpu") != digestFlags("fpu  avx\tsse2") {
		t.Error("the flag digest depends on order or on whitespace, and must depend on neither")
	}
}

func TestParseFirecrackerVersion(t *testing.T) {
	for name, testCase := range map[string]struct{ output, want string }{
		"the ordinary shape": {"Firecracker v1.13.1\n", "v1.13.1"},
		"with a second line": {
			"Firecracker v1.4.1\nSupported snapshot data format versions: 1.4.1\n", "v1.4.1",
		},
		// Any stable string works: the comparison is equality, so an unexpected
		// shape is kept whole rather than thrown away.
		"an unexpected shape": {"firecracker 1.13.1\n", "firecracker 1.13.1"},
		"nothing at all":      {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if version := parseFirecrackerVersion(testCase.output); version != testCase.want {
				t.Errorf("parseFirecrackerVersion(%q) = %q, want %q", testCase.output, version, testCase.want)
			}
		})
	}
}

// A host with no Firecracker cannot say what its snapshots were captured on. It
// fails loudly instead of signing a signature with an empty version — two hosts
// that both failed to answer would otherwise produce signatures that match each
// other, and a warm restore onto the wrong CPU is exactly what the signature
// exists to prevent.
func TestReadFailsWhenFirecrackerIsMissing(t *testing.T) {
	fake := healthyHost()
	fake.errors[firecrackerRead] = errors.New(
		"fork/exec /usr/local/bin/firecracker: no such file or directory")

	facts, err := readWith(t, fake)

	if err == nil {
		t.Fatalf("Read succeeded with no Firecracker installed, and returned %+v", facts)
	}
	if !strings.Contains(err.Error(), firecrackerBinary) {
		t.Errorf("error %q does not name the binary it could not run", err)
	}
}

// The exit code is discarded but the answer is not optional: a Firecracker that
// printed nothing has not answered.
func TestReadFailsWhenFirecrackerPrintsNoVersion(t *testing.T) {
	fake := healthyHost()
	fake.outputs[firecrackerRead] = ""

	if facts, err := readWith(t, fake); err == nil {
		t.Fatalf("Read succeeded with an empty version, and returned %+v", facts)
	}
}

// A Firecracker that prints its version and then exits non-zero has still
// answered the question, so this is the one read whose exit code is discarded —
// the guarded `|| true`, matching the Python's unchecked subprocess.run. The
// "- " prefix is the fake recording that choice; every other read here aborts on
// a non-zero exit.
func TestTheVersionReadDiscardsTheExitCode(t *testing.T) {
	fake := healthyHost()

	facts, err := readWith(t, fake)

	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !slices.Contains(fake.trace, "- "+firecrackerRead) {
		t.Errorf("trace %v does not read the version with its exit code discarded", fake.trace)
	}
	if facts.FirecrackerVersion != "v1.13.1" {
		t.Errorf("FirecrackerVersion = %q, want v1.13.1", facts.FirecrackerVersion)
	}
}
