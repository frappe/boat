package hostfacts

import (
	"context"
	"errors"
	"fmt"
	"github.com/frappe/boat/internal/run"
	"strings"
	"testing"

	"github.com/frappe/boat/internal/model"
	"github.com/frappe/boat/internal/version"
)

// These tests drive real canned host output through the command seam: a real
// `free -m` table, a real two-block /proc/cpuinfo, real lvs padding. The parsing
// is the whole of this package, and it is the half that bites on real hosts.

const (
	unameNode       = "uname -n"
	unameRelease    = "uname -r"
	processorCount  = "nproc --all"
	memoryRead      = "env LC_ALL=C free -m"
	poolSizeRead    = "sudo lvs --noheadings --nosuffix --units b -o lv_size atlas/pool0"
	poolUsageRead   = "sudo lvs --noheadings -o data_percent atlas/pool0"
	cpuinfoRead     = "cat /proc/cpuinfo"
	firecrackerRead = "/usr/local/bin/firecracker --version"
)

// freeOutput is `free -m` from procps-ng 4: a header row and a padded Mem: row.
const freeOutput = `               total        used        free      shared  buff/cache   available
Mem:           15924        2871        9182         113        4238       12766
Swap:              0           0           0
`

// cpuinfoOutput is the first two processor blocks of /proc/cpuinfo on a
// DigitalOcean droplet, tab separators and all. The second block deliberately
// disagrees with the first: only the first is read.
const cpuinfoOutput = `processor	: 0
vendor_id	: GenuineIntel
cpu family	: 6
model		: 85
model name	: Intel(R) Xeon(R) Gold 6140 CPU @ 2.30GHz
stepping	: 4
microcode	: 0x1
cpu MHz		: 2294.608
cache size	: 25344 KB
physical id	: 0
siblings	: 8
core id		: 0
cpu cores	: 8
fpu		: yes
cpuid level	: 13
wp		: yes
flags		: fpu vme de pse tsc msr pae mce cx8 apic sep mtrr pge mca cmov pat pse36 clflush mmx fxsr sse sse2 ss ht syscall nx pdpe1gb rdtscp lm constant_tsc arch_perfmon rep_good nopl xtopology cpuid tsc_known_freq pni pclmulqdq vmx ssse3 fma cx16 pcid sse4_1 sse4_2 x2apic movbe popcnt tsc_deadline_timer aes xsave avx f16c rdrand hypervisor lahf_lm abm 3dnowprefetch cpuid_fault invpcid_single pti ssbd ibrs ibpb stibp tpr_shadow vnmi flexpriority ept vpid ept_ad fsgsbase tsc_adjust bmi1 hle avx2 smep bmi2 erms invpcid rtm mpx avx512f avx512dq rdseed adx smap clflushopt clwb avx512cd avx512bw avx512vl xsaveopt xsavec xgetbv1 xsaves arat pku ospke md_clear flush_l1d arch_capabilities
bugs		: cpu_meltdown spectre_v1 spectre_v2 spec_store_bypass l1tf mds swapgs taa itlb_multihit mmio_stale_data
bogomips	: 4589.21
clflush size	: 64
cache_alignment	: 64
address sizes	: 46 bits physical, 48 bits virtual
power management:

processor	: 1
vendor_id	: GenuineIntel
model name	: Some Other CPU That Must Not Win
microcode	: 0xdeadbeef
flags		: nothing_here
power management:
`

// flagsDigest is sha256(sorted flags joined by " ")[:16] for the block above,
// computed with the Python this package ports rather than with the code under
// test — a digest a port checks against itself proves nothing.
const flagsDigest = "9d5c7a66d16f2223"

// emptyFlagsDigest is the same recipe applied to no flags at all: sha256("").
const emptyFlagsDigest = "e3b0c44298fc1c14"

// fakeCommands answers each rendered command from a script and records the
// sequence. A recorded line carries "- " when the command's exit code was
// discarded, so the trace shows which reads tolerate a non-zero exit.
type fakeCommands struct {
	trace   []string
	outputs map[string]string
	errors  map[string]error
}

func (fake *fakeCommands) Run(_ context.Context, template string, parameters ...any) (string, error) {
	command := render(template, parameters...)
	fake.trace = append(fake.trace, command)
	return fake.outputs[command], fake.errors[command]
}

func (fake *fakeCommands) RunUnchecked(
	_ context.Context, template string, parameters ...any,
) (string, error) {
	command := render(template, parameters...)
	fake.trace = append(fake.trace, "- "+command)
	return fake.outputs[command], fake.errors[command]
}

// healthyHost is an ordinary 16 GB droplet with a 200 GB thin pool, a third full.
func healthyHost() *fakeCommands {
	return &fakeCommands{
		outputs: map[string]string{
			unameNode:       "atlas-host-3\n",
			unameRelease:    "6.14.0-24-generic\n",
			processorCount:  "8\n",
			memoryRead:      freeOutput,
			poolSizeRead:    "  209715200000\n",
			poolUsageRead:   "  37.42\n",
			cpuinfoRead:     cpuinfoOutput,
			firecrackerRead: "Firecracker v1.13.1\n",
		},
		errors: map[string]error{},
	}
}

// render substitutes each {} with its parameter the way run.Substitute does,
// minus the shell quoting — every value here is a path or an LVM reference, and
// an unquoted line is the one a reader compares to the Python by eye.
func render(template string, parameters ...any) string {
	parts := strings.Split(template, "{}")
	if len(parts)-1 != len(parameters) {
		panic(fmt.Sprintf("%q: %d placeholders, %d parameters", template, len(parts)-1, len(parameters)))
	}
	var builder strings.Builder
	for index, part := range parts {
		builder.WriteString(part)
		if index < len(parameters) {
			fmt.Fprintf(&builder, "%v", parameters[index])
		}
	}
	return builder.String()
}

func readWith(t *testing.T, fake *fakeCommands) (model.HostFacts, error) {
	t.Helper()
	return read(context.Background(), fake)
}

func TestReadMeasuresTheLiveHost(t *testing.T) {
	fake := healthyHost()

	facts, err := readWith(t, fake)

	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, check := range []struct {
		field string
		got   any
		want  any
	}{
		{"Hostname", facts.Hostname, "atlas-host-3"},
		{"KernelVersion", facts.KernelVersion, "6.14.0-24-generic"},
		{"FirecrackerVersion", facts.FirecrackerVersion, "v1.13.1"},
		{"BoatVersion", facts.BoatVersion, version.Version},
		{"VCPUsTotal", facts.VCPUsTotal, 8},
		{"MemoryMegabytesTotal", facts.MemoryMegabytesTotal, 15924},
		{"MemoryMegabytesFree", facts.MemoryMegabytesFree, 12766},
		// 209715200000 B / 1 GiB = 195.31, truncated: a host never claims a
		// gigabyte it cannot hand out.
		{"PoolDiskGigabytesTotal", facts.PoolDiskGigabytesTotal, 195},
		{"PoolUsedPercent", facts.PoolUsedPercent, float32(37.42)},
	} {
		if check.got != check.want {
			t.Errorf("%s = %v, want %v", check.field, check.got, check.want)
		}
	}
}

// The command sequence is the whole of the behaviour on a machine with no LVM
// and no Firecracker, and it is what a differential test against the Python
// compares. Only the version read tolerates a non-zero exit.
func TestReadAsksTheHostInAFixedOrder(t *testing.T) {
	fake := healthyHost()

	if _, err := readWith(t, fake); err != nil {
		t.Fatalf("Read: %v", err)
	}
	expected := []string{
		unameNode, unameRelease, processorCount, memoryRead,
		poolSizeRead, poolUsageRead, cpuinfoRead, "- " + firecrackerRead,
	}
	if len(fake.trace) != len(expected) {
		t.Fatalf("command sequence:\ngot:\n  %s\nwant:\n  %s",
			strings.Join(fake.trace, "\n  "), strings.Join(expected, "\n  "))
	}
	for index := range expected {
		if fake.trace[index] != expected[index] {
			t.Errorf("command %d:\ngot:  %s\nwant: %s", index, fake.trace[index], expected[index])
		}
	}
}

// The host signature is the JSON document a restore compares against, so its
// five keys and their spelling are the port's contract with every signature
// already captured on an Atlas host.
func TestReadComposesTheHostSignatureFromCPUKernelAndFirecracker(t *testing.T) {
	facts, err := readWith(t, healthyHost())

	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	expected := `{"cpu_model":"Intel(R) Xeon(R) Gold 6140 CPU @ 2.30GHz",` +
		`"microcode":"0x1","cpu_flags_sha256":"` + flagsDigest + `",` +
		`"kernel":"6.14.0-24-generic","firecracker":"v1.13.1"}`
	if facts.HostSignature != expected {
		t.Errorf("host signature:\ngot:  %s\nwant: %s", facts.HostSignature, expected)
	}
}

// A fact Boat cannot read is an error, not a zero: a zero vCPU count or an empty
// signature reads downstream as a real answer, and both are decisions taken on a
// fact nobody has.
func TestReadFailsLoudlyOnEveryUnreadableFact(t *testing.T) {
	for _, command := range []string{
		unameNode, unameRelease, processorCount, memoryRead, poolSizeRead, poolUsageRead, cpuinfoRead,
	} {
		t.Run(command, func(t *testing.T) {
			fake := healthyHost()
			fake.errors[command] = errors.New("host unreachable")

			facts, err := readWith(t, fake)

			if err == nil {
				t.Fatalf("Read succeeded with %q failing, and returned %+v", command, facts)
			}
			if (facts != model.HostFacts{}) {
				t.Errorf("Read returned a half-measured host: %+v", facts)
			}
		})
	}
}

// A host with no `atlas` volume group still reports every other fact.
//
// This is the ordinary state of a machine that has not been bootstrapped, one
// mid-bootstrap, and one whose pool has broken — and the last is exactly when an
// operator most needs to see the rest of the host. Failing the whole read there
// made GET /export answer 500 on a real unbootstrapped host, which is how this
// was found.
//
// Note what is NOT tolerated, and is covered by the test above: a generic
// failure to run lvs at all. lvs answering "no such volume group" is the host
// describing itself; lvs not answering is the host failing to.
func TestAHostWithNoThinPoolStillReportsItsOtherFacts(t *testing.T) {
	for _, command := range []string{poolSizeRead, poolUsageRead} {
		t.Run(command, func(t *testing.T) {
			fake := healthyHost()
			fake.errors[command] = &run.CommandError{
				Argv:     []string{"lvs"},
				ExitCode: 5,
				Output:   `  Volume group "atlas" not found`,
			}

			facts, err := readWith(t, fake)

			if err != nil {
				t.Fatalf("a host with no thin pool failed its whole read: %v", err)
			}
			if facts.VCPUsTotal == 0 || facts.MemoryMegabytesTotal == 0 {
				t.Errorf("the readable facts were dropped too: %+v", facts)
			}
			if facts.PoolDiskGigabytesTotal != 0 {
				t.Errorf("pool total = %d, want 0 — absent is not a measurement", facts.PoolDiskGigabytesTotal)
			}
		})
	}
}
