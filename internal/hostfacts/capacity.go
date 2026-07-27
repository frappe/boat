package hostfacts

import (
	"context"
	"errors"
	"fmt"
	"github.com/frappe/boat/internal/run"
	"log/slog"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/model"
)

// The volume group and thin pool every per-VM disk is a CoW snapshot inside.
// Atlas's ThinPool defaults, inherited rather than chosen: Boat adopts hosts
// whose pool already exists under these names.
const (
	volumeGroup   = "atlas"
	poolName      = "pool0"
	poolReference = volumeGroup + "/" + poolName
)

// gigabyte is GiB — the unit every disk number in Atlas is counted in.
const gigabyte = 1 << 30

// addCapacity reads the three physical totals placement packs against — CPU
// threads, RAM, and the size of the thin pool the VM disks draw from — plus the
// pool's live fullness.
func addCapacity(ctx context.Context, commands commands, facts *model.HostFacts) error {
	if err := addProcessors(ctx, commands, facts); err != nil {
		return err
	}
	if err := addMemory(ctx, commands, facts); err != nil {
		return err
	}
	return addPool(ctx, commands, facts)
}

// addProcessors counts the host's logical CPUs, which is what os.cpu_count()
// reported in the Python.
//
// `--all` and not a bare `nproc`: bare nproc reports the CPUs available to the
// calling process, so a Boat started under a restrictive CPUAffinity or inside a
// cpuset would report a host smaller than it is, and Atlas would quietly stop
// packing onto hardware it already paid for.
func addProcessors(ctx context.Context, commands commands, facts *model.HostFacts) error {
	output, err := line(ctx, commands, "nproc --all")
	if err != nil {
		return fmt.Errorf("counting processors: %w", err)
	}
	count, err := strconv.Atoi(output)
	if err != nil {
		return fmt.Errorf("nproc --all printed %q: %w", output, err)
	}
	facts.VCPUsTotal = count
	return nil
}

// addMemory reads RAM through `free`, under a pinned locale: procps translates
// its column headings, and parseMemory finds its columns by name. LC_ALL is set
// with env(1) rather than in the daemon's own environment so the pinning is
// visible in the command trace. lvs below carries the same hazard in its decimal
// separator and does NOT get the same treatment: it runs under sudo, and a
// `sudo env ...` line in sudoers.d/boat grants env — which runs any binary — for
// a bug that surfaces as a loud parse failure anyway.
func addMemory(ctx context.Context, commands commands, facts *model.HostFacts) error {
	output, err := commands.Run(ctx, "env LC_ALL=C free -m")
	if err != nil {
		return fmt.Errorf("reading memory: %w", err)
	}
	memory, err := parseMemory(output)
	if err != nil {
		return err
	}
	facts.MemoryMegabytesTotal, facts.MemoryMegabytesFree = memory.total, memory.available
	return nil
}

// memory is what one `free -m` read said, in whole MiB. total is the same
// quantity the Python took from /proc/meminfo's MemTotal line (kB integer-
// divided to MB), so the number Atlas has been stamping does not move.
type memory struct{ total, available int }

// parseMemory reads free's `Mem:` row by looking its columns up in the header
// rather than by position, so a procps that grows a column does not silently
// shift the numbers by one — the failure mode of a positional parse is not an
// error, it is a wrong capacity.
//
// available, not free: `free` counts reclaimable page cache as used, so a host
// that has been up a week reports a few hundred free MB while most of its RAM is
// available. Placement reading the free column would decide a mostly empty host
// is full. This is MemAvailable, the same number Atlas's image builder already
// gates a build on.
func parseMemory(output string) (memory, error) {
	var header, row []string
	for _, text := range strings.Split(output, "\n") {
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "total":
			header = fields
		case "Mem:":
			row = fields[1:]
		}
	}
	total, err := column(header, row, "total")
	if err != nil {
		return memory{}, err
	}
	available, err := column(header, row, "available")
	if err != nil {
		return memory{}, err
	}
	return memory{total: total, available: available}, nil
}

// column returns the Mem: row's value under the named heading. A heading free
// does not print — `available` is absent on procps older than 3.3.10 — is an
// error rather than a zero, because a zero here is a host that looks full.
func column(header, row []string, name string) (int, error) {
	for index, heading := range header {
		if heading != name {
			continue
		}
		if index >= len(row) {
			return 0, fmt.Errorf("free: the Mem: row has no %q value", name)
		}
		value, err := strconv.Atoi(row[index])
		if err != nil {
			return 0, fmt.Errorf("free: %q under %q: %w", row[index], name, err)
		}
		return value, nil
	}
	return 0, fmt.Errorf("free: no %q column in its output", name)
}

// addPool reads the thin pool, and does NOT fail the host's facts when it
// cannot.
//
// A host with no `atlas` volume group is a real and ordinary state: one that has
// not been bootstrapped yet, one mid-bootstrap, and — the case that matters —
// one whose pool has broken. Failing the whole read there made `GET /export`
// answer 500, so the single host state where an operator most needs to see what
// the machine looks like was the one state Boat refused to describe. Observed
// state is meant to report what is there; "there is no pool" is an observation,
// not an error.
//
// The totals are left at zero rather than guessed, and zero is how Atlas's
// mirror already recognises a fact it must not overwrite a measured value with.
func addPool(ctx context.Context, commands commands, facts *model.HostFacts) error {
	size, err := poolSize(ctx, commands)
	if err != nil {
		return tolerateAbsentPool("size", err)
	}
	used, err := poolUsedPercent(ctx, commands)
	if err != nil {
		return tolerateAbsentPool("fullness", err)
	}
	facts.PoolDiskGigabytesTotal, facts.PoolUsedPercent = int(size/gigabyte), used
	return nil
}

// tolerateAbsentPool separates a pool that is not there from one that answered
// something unreadable.
//
// lvs exiting non-zero is how a host says "no such volume group", which is the
// ordinary state of a machine that has not been bootstrapped, is mid-bootstrap,
// or whose pool has broken — and the last of those is exactly when an operator
// most needs the rest of the host's facts. That is reported and tolerated.
//
// A parse failure is not tolerated. lvs answering with something this code
// cannot read means the host's inventory is not what it claims to be, and
// guessing past that is how a placement decision gets made against a number
// nobody actually measured.
func tolerateAbsentPool(what string, err error) error {
	var commandError *run.CommandError
	if !errors.As(err, &commandError) {
		return err
	}
	slog.Warn("this host has no readable thin pool", "fact", what, "error", err)
	return nil
}

// poolSize is the thin pool's data capacity in bytes. For a loopback-backed pool
// that is the sparse overcommit ceiling (ATLAS_POOL_DATA_SIZE); for a real
// device PV it is the disk the pool was carved from. Either way it is the disk
// budget reserved VM disks are packed against — never the host's root
// filesystem, which is a different disk and a different number.
func poolSize(ctx context.Context, commands commands) (int64, error) {
	output, err := line(ctx, commands, "sudo lvs --noheadings --nosuffix --units b -o lv_size {}", poolReference)
	if err != nil {
		return 0, fmt.Errorf("reading the thin pool's size: %w", err)
	}
	size, err := strconv.ParseInt(output, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("lvs printed %q for %s's size: %w", output, poolReference, err)
	}
	return size, nil
}

// poolUsedPercent is the pool's live data fill. It is an alert signal and never
// a placement predicate: a thin snapshot is free up front and pays for itself on
// later CoW writes, so this number tells an operator the pool is filling, not
// whether one more VM fits.
func poolUsedPercent(ctx context.Context, commands commands) (float32, error) {
	output, err := line(ctx, commands, "sudo lvs --noheadings -o data_percent {}", poolReference)
	if err != nil {
		return 0, fmt.Errorf("reading the thin pool's fullness: %w", err)
	}
	// lvs prints a localized decimal, which is a hazard the Python original names
	// too: on a host whose locale uses a comma this fails to parse, loudly, which
	// is the correct failure — a percentage read wrong is an alert missed.
	//
	// A blank column is not a parse failure: lvs prints nothing for a pool that
	// is not active and so has no usage to report. That is the `${data_pct:-0}`
	// default the shell original carried, and it is the honest answer — a pool
	// nothing has been allocated from is 0% full.
	if output == "" {
		return 0, nil
	}
	percent, err := strconv.ParseFloat(output, 32)
	if err != nil {
		return 0, fmt.Errorf("lvs printed %q for %s's data_percent: %w", output, poolReference, err)
	}
	return float32(percent), nil
}
