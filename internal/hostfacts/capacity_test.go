package hostfacts

import (
	"strings"
	"testing"
)

func TestParseMemoryReadsTheColumnsFreeLabelled(t *testing.T) {
	parsed, err := parseMemory(freeOutput)

	if err != nil {
		t.Fatalf("parseMemory: %v", err)
	}
	if parsed.total != 15924 {
		t.Errorf("total = %d, want 15924", parsed.total)
	}
	// Not 9182, the `free` column: page cache is reclaimable, and a host with a
	// week of uptime has almost none of its RAM in that column while most of it
	// is still available.
	if parsed.available != 12766 {
		t.Errorf("available = %d, want 12766 (the available column, not free)", parsed.available)
	}
}

// procps older than 3.3.10 prints no `available` column at all. There is no safe
// number to substitute — free would understate the host by gigabytes and
// buff/cache would overstate it — so the read fails and names what it wanted.
func TestParseMemoryFailsOnFreeWithNoAvailableColumn(t *testing.T) {
	old := `             total       used       free     shared    buffers     cached
Mem:          7986       7654        331          0        123       4567
-/+ buffers/cache:       2964       5021
Swap:         2047          0       2047
`

	_, err := parseMemory(old)

	if err == nil {
		t.Fatal("parseMemory succeeded on procps output with no available column")
	}
	if !strings.Contains(err.Error(), "available") {
		t.Errorf("error %q does not name the missing column", err)
	}
}

func TestParseMemoryFailsOnAMalformedRow(t *testing.T) {
	for name, output := range map[string]string{
		"non-numeric value": `               total        used        free      shared  buff/cache   available
Mem:           15924        2871        9182         113        4238         n/a
`,
		"truncated row": `               total        used        free      shared  buff/cache   available
Mem:           15924
`,
		"no Mem: row at all": `               total        used        free      shared  buff/cache   available
Swap:              0           0           0
`,
		"empty": "",
	} {
		t.Run(name, func(t *testing.T) {
			if parsed, err := parseMemory(output); err == nil {
				t.Fatalf("parseMemory = %+v, want an error", parsed)
			}
		})
	}
}

func TestPoolFullnessIsReadFromLvs(t *testing.T) {
	for name, testCase := range map[string]struct {
		output string
		want   float32
	}{
		"a third full":     {"  37.42\n", 37.42},
		"near full":        {"  97.85\n", 97.85},
		"completely full":  {" 100.00\n", 100},
		"nothing yet":      {"  0.00\n", 0},
		"an inactive pool": {"  \n", 0},
	} {
		t.Run(name, func(t *testing.T) {
			fake := healthyHost()
			fake.outputs[poolUsageRead] = testCase.output

			facts, err := readWith(t, fake)

			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if facts.PoolUsedPercent == nil || *facts.PoolUsedPercent != testCase.want {
				t.Errorf("PoolUsedPercent = %v, want %v", facts.PoolUsedPercent, testCase.want)
			}
		})
	}
}

// A pool at 97% is a host an operator has to be paged about, not one Boat may
// round into "fine". The number is reported exactly as lvs gave it.
func TestANearFullPoolIsReportedNotRounded(t *testing.T) {
	fake := healthyHost()
	fake.outputs[poolUsageRead] = "  97.85\n"
	fake.outputs[poolSizeRead] = "  214748364800\n"

	facts, err := readWith(t, fake)

	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if facts.PoolUsedPercent == nil || *facts.PoolUsedPercent != 97.85 || facts.PoolDiskGigabytesTotal == nil || *facts.PoolDiskGigabytesTotal != 200 {
		t.Errorf("pool = %d GB at %v%%, want 200 GB at 97.85%%",
			facts.PoolDiskGigabytesTotal, facts.PoolUsedPercent)
	}
}

// lvs printing something unparseable is a broken read, and a broken read is not
// an empty pool: reporting 0 GB would take the host out of placement, and
// reporting 0% would hide one that is about to wedge.
func TestAMalformedLvsLineFailsTheRead(t *testing.T) {
	for name, testCase := range map[string]struct{ command, output string }{
		"size with a units suffix": {poolSizeRead, "  200.00g\n"},
		"size is a message":        {poolSizeRead, "  Volume group \"atlas\" not found\n"},
		"percent is not a number":  {poolUsageRead, "  n/a\n"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := healthyHost()
			fake.outputs[testCase.command] = testCase.output

			facts, err := readWith(t, fake)

			if err == nil {
				t.Fatalf("Read succeeded on %q, and returned %+v", testCase.output, facts)
			}
			if !strings.Contains(err.Error(), poolReference) {
				t.Errorf("error %q does not name the pool it failed on", err)
			}
		})
	}
}

func TestProcessorCountFailsOnAnythingButANumber(t *testing.T) {
	fake := healthyHost()
	fake.outputs[processorCount] = "nproc: invalid option -- 'x'\n"

	if facts, err := readWith(t, fake); err == nil {
		t.Fatalf("Read succeeded with an unparseable nproc, and returned %+v", facts)
	}
}
