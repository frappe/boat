package migration

import (
	"context"
	"testing"
)

const (
	exportBasePidFile = "/var/lib/atlas/run/migrate-nbd-20000.pid"
	exportMetaPidFile = "/var/lib/atlas/run/migrate-nbd-20001.pid"
	exportMetaTar     = "/var/lib/atlas/run/migrate-base-meta-" + testImage + ".tar"
)

// Both exports come down by pidfile, and the staged tar with them. The base LV is
// never named: it is the source's own immutable image and this verb must not be
// able to touch it.
func TestExportCleanupSourceStopsBothExports(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo test -f "+exportBasePidFile).
		output("sudo cat "+exportBasePidFile, "4242\n").
		exists("sudo test -f "+exportMetaPidFile).
		output("sudo cat "+exportMetaPidFile, "4243\n")

	err := ExportCleanupSource(context.Background(), fake, ExportCleanupSourceParams{
		ImageName: testImage, NBDPort: 20000,
	})
	if err != nil {
		t.Fatalf("ExportCleanupSource: %v", err)
	}

	assertTrace(t, fake,
		// The rootfs LV export, killed by the pid its own pidfile holds — this verb
		// did not start the export, so the pidfile is the only handle it has.
		"? sudo test -f "+exportBasePidFile,
		"- sudo cat "+exportBasePidFile,
		"- sudo kill 4242",
		"- sudo rm -f "+exportBasePidFile,
		// The image-dir tar export, one port up.
		"? sudo test -f "+exportMetaPidFile,
		"- sudo cat "+exportMetaPidFile,
		"- sudo kill 4243",
		"- sudo rm -f "+exportMetaPidFile,
		// The staged tar, keyed by image name so a concurrent export's is left alone.
		"- sudo rm -f "+exportMetaTar,
	)
	for _, line := range fake.trace {
		if line == "sudo lvremove -f atlas/"+baseImageLV(testImage) {
			t.Error("the source's own base image LV was removed")
		}
	}
}

// A re-entry after a partial cleanup finishes the rest: a pidfile that is already
// gone is a no-op, not a failure.
func TestExportCleanupSourceIsIdempotent(t *testing.T) {
	fake := newFakeCommands() // no pidfiles left

	err := ExportCleanupSource(context.Background(), fake, ExportCleanupSourceParams{
		ImageName: testImage, NBDPort: 20000,
	})
	if err != nil {
		t.Fatalf("ExportCleanupSource: %v", err)
	}

	assertTrace(t, fake,
		"? sudo test -f "+exportBasePidFile,
		"? sudo test -f "+exportMetaPidFile,
		"- sudo rm -f "+exportMetaTar,
	)
}

// Port 0 means the export never started, so there is nothing to stop — only the
// staged tar is swept, in case a partial export left one.
func TestExportCleanupSourceWithNoPortOnlySweepsTheTar(t *testing.T) {
	fake := newFakeCommands()

	err := ExportCleanupSource(context.Background(), fake, ExportCleanupSourceParams{ImageName: testImage})
	if err != nil {
		t.Fatalf("ExportCleanupSource: %v", err)
	}

	assertTrace(t, fake, "- sudo rm -f "+exportMetaTar)
}
