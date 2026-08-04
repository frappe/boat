package migration

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/run"
)

// The dm-clone host surface and the pure parsers of its table/status lines. A
// dm-clone serves reads from the source (an NBD-backed snapshot) until a region is
// hydrated, lands writes on the local dest LV, and — once messaged enable_hydration
// — copies every region in the background; at 100% it can be collapsed. Shared by
// clone-target, receive-base, poll-hydration and the collapse.

// ensureDMClone creates the dm-clone mapping if absent: a small zeroed metadata LV,
// then `dmsetup create <clone> --table "0 <sectors> clone <meta> <dest> <source>
// <region>"`. Idempotent — skips if the mapper device already exists. Ports the
// _ensure_dm_clone shared by clone-target and receive-base.
func ensureDMClone(ctx context.Context, cmd commands, cloneName, metaName, destName, sourceDevice string) error {
	if cmd.OK(ctx, "sudo dmsetup info {}", cloneName) {
		return nil // already created (idempotent)
	}
	// dm-clone needs ~(dev_size / region_size) bits of metadata; 16 MiB covers any VM
	// disk we host. Zero it once — dm-clone refuses stale metadata.
	if !lvExists(ctx, cmd, metaName) {
		if err := createThin(ctx, cmd, metaName, 1); err != nil {
			return err
		}
		if _, err := cmd.Run(ctx, "sudo dd if=/dev/zero of={} bs=1M count=16 conv=fsync", lvDevicePath(metaName)); err != nil {
			return err
		}
	}
	sizeBytes, err := lvSizeBytes(ctx, cmd, destName)
	if err != nil {
		return err
	}
	table := fmt.Sprintf(
		"0 %d clone %s %s %s %d",
		sizeBytes/512, lvDevicePath(metaName), lvDevicePath(destName), sourceDevice, regionSectors,
	)
	_, err = cmd.Run(ctx, "sudo dmsetup create {} --table {}", cloneName, table)
	return err
}

// dropCloneIfSourceDead tears a dm-clone down ONLY when its source nbd client has
// died — the wedged state that freezes hydration (source reads return 0 bytes)
// while dmsetup still reports the clone present. Removing it frees the nbd device so
// the client can be re-dialed and the clone rebuilt (the clone pins the device open,
// so the client cannot be disconnected until the clone comes down — proven on a real
// f1→f2 migration). A HEALTHY clone is left untouched: this is idempotent and must
// not discard good hydration progress. The rebuilt clone re-hydrates from 0 —
// correctness over speed. Ports the _drop_clone_if_source_dead shared helper.
func dropCloneIfSourceDead(ctx context.Context, cmd commands, cloneName string, slot int) {
	if !cmd.OK(ctx, "sudo dmsetup info {}", cloneName) {
		return // no clone yet — nothing wedged
	}
	if nbdClientAlive(ctx, cmd, slot) {
		return // source client alive — healthy, leave it
	}
	cmd.RunUnchecked(ctx, "sudo dmsetup remove {}", cloneName)
}

const nbdMajor = "43" // Linux block major for /dev/nbd*

// cloneSourceAlive reports whether the nbd client backing a dm-clone is still alive
// — the REPORTED health poll-hydration surfaces so the controller re-runs prepare
// (rebuilds the stack) on a dead source. This is the one migration check that is
// three-valued: a `/proc/<pid>` probe nobody could make must NOT collapse into
// "dead" and trigger a destructive rebuild, so an Unknown is returned as an error
// and the poll re-runs rather than acting on a guess.
//
// dm-clone cannot be asked its source directly, so the clone's live table is read —
// its 6th field is the source as major:minor (dmsetup reports device NUMBERS, e.g.
// 43:0 for nbd0) — resolved to /sys/block/nbdN and the nbd's owning pid checked. A
// non-nbd source (already collapsed) counts as alive: nothing to heal.
func cloneSourceAlive(ctx context.Context, cmd commands, cloneName string) (bool, error) {
	table, err := cmd.RunUnchecked(ctx, "sudo dmsetup table {}", cloneName)
	if err != nil {
		return false, err
	}
	source := cloneTableSource(table)
	major, _, found := strings.Cut(source, ":")
	if !found || major != nbdMajor {
		return true, nil // not nbd-backed (e.g. collapsed) — nothing to heal
	}
	link, err := cmd.RunUnchecked(ctx, "readlink /sys/dev/block/{}", source)
	if err != nil {
		return false, err
	}
	block := path.Base(strings.TrimSpace(link))
	if !strings.HasPrefix(block, "nbd") {
		return true, nil
	}
	output, err := cmd.RunUnchecked(ctx, "cat /sys/block/{}/pid", block)
	if err != nil {
		return false, err
	}
	pid := strings.TrimSpace(output)
	if !isDigits(pid) {
		return false, nil // no owner recorded → the client died (a proven negative)
	}
	answer, err := cmd.Probe(ctx, "test -d /proc/{}", pid)
	if err != nil {
		return false, err // could not look — surfaced, not rounded to "dead"
	}
	return answer == run.Yes, nil
}

// cloneTableSource is the source device (field 5) of a dm-clone table line:
// "0 <len> clone <meta> <dest> <SOURCE> <region> ...". "" when the line is not a
// clone table (already collapsed to linear, or empty).
func cloneTableSource(tableLine string) string {
	fields := strings.Fields(tableLine)
	if len(fields) > 5 {
		return fields[5]
	}
	return ""
}

// cloneTableDest is the write-target device (field 4) of a dm-clone table line:
// "0 <len> clone <meta> <DEST> <source> <region> …" — the LV holding every hydrated
// block, and the device the collapse reloads a linear map onto. Read from the LIVE
// table rather than reconstructed from the UUID: the Python cutover-target rebuilds
// it as /dev/atlas/atlas-vm-<key>, which is correct for the root disk but names a
// non-existent LV for the data disk (whose dest is atlas-data-<uuid>, not
// atlas-vm-<uuid>-data). Field 4 is what clone-target actually wrote, so it is right
// for both. "" when the line is not a clone table.
func cloneTableDest(tableLine string) string {
	fields := strings.Fields(tableLine)
	if len(fields) > 4 {
		return fields[4]
	}
	return ""
}

// hydrationPercent parses a dm-clone status line into a 0..100 percent. The kernel
// prints: <meta_block> <#used>/<#total_meta> <region_size> <#hydrated>/<#total_regions> …
// so the 2nd "a/b" whitespace field is hydrated/total. Isolated + pure so the parse
// (the bit that breaks on a kernel format change) is unit-testable with no dm stack.
func hydrationPercent(statusLine string) (int, error) {
	hydrated, total, err := hydratedTotal(statusLine)
	if err != nil {
		return 0, err
	}
	if total == 0 {
		return 100, nil
	}
	percent := hydrated * 100 / total
	if percent > 100 {
		percent = 100
	}
	return int(percent), nil
}

// fullyHydrated reports whether every region is copied — the guard before a collapse
// (collapsing early strands un-copied blocks behind a torn-down NBD, reading as
// zeros). A status that cannot be parsed is NOT fully hydrated: a collapse refused on
// an unreadable status is safe, a collapse allowed on one is not.
func fullyHydrated(statusLine string) bool {
	hydrated, total, err := hydratedTotal(statusLine)
	return err == nil && total > 0 && hydrated >= total
}

// hydratedTotal pulls the hydrated/total region pair (the 2nd "a/b" field) out of a
// dm-clone status line. The first "a/b" is the metadata block usage; a line with
// fewer than two such pairs is not a clone status.
func hydratedTotal(statusLine string) (int64, int64, error) {
	var pairs [][2]int64
	for _, field := range strings.Fields(statusLine) {
		numerator, denominator, found := strings.Cut(field, "/")
		if !found {
			continue
		}
		left, leftErr := strconv.ParseInt(numerator, 10, 64)
		right, rightErr := strconv.ParseInt(denominator, 10, 64)
		if leftErr != nil || rightErr != nil {
			continue
		}
		pairs = append(pairs, [2]int64{left, right})
	}
	if len(pairs) < 2 {
		return 0, 0, fmt.Errorf("cannot parse dm-clone hydration from %q", statusLine)
	}
	return pairs[1][0], pairs[1][1], nil
}

// isLinearTable reports whether a dm table is already a `linear` target — a clone
// that has been collapsed, so a re-entered collapse is a no-op. "0 <sectors> linear
// <dest> 0": the target keyword is field 2.
func isLinearTable(tableLine string) bool {
	fields := strings.Fields(tableLine)
	return len(fields) >= 3 && fields[2] == "linear"
}

// cloneSectors is the total sector count from a dm table line ("0 <sectors> …"),
// reused when a clone table is reloaded as a linear one of the same length.
func cloneSectors(tableLine string) (int64, error) {
	fields := strings.Fields(tableLine)
	if len(fields) < 2 {
		return 0, fmt.Errorf("cannot read sector count from dm table %q", tableLine)
	}
	sectors, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("dm table sector count not a number: %q", tableLine)
	}
	return sectors, nil
}
