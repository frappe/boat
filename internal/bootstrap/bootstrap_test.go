package bootstrap

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTheResultCarriesEveryKeyThePythonEmits is the contract check, and it is
// worth its length for the same reason cmd/boat's flag table is: Atlas parses
// this payload with `cls(**payload)` against the BootstrapResult dataclass, so a
// key that is missing, extra or misspelled is a TypeError on the CONTROLLER,
// after the whole bootstrap has already run on the host. The names below are read
// off scripts/bootstrap-server.py's BootstrapResult, field for field.
func TestTheResultCarriesEveryKeyThePythonEmits(t *testing.T) {
	wanted := []string{
		"firecracker_version",
		"jailer_version",
		"kernel_version",
		"architecture",
		"python_version",
		"vcpus_total",
		"memory_megabytes_total",
		"pool_disk_gigabytes_total",
	}

	fields := Result{}.Fields()

	for _, key := range wanted {
		if _, present := fields[key]; !present {
			t.Errorf("the result has no %q; the controller's dataclass would raise on it", key)
		}
	}
	if len(fields) != len(wanted) {
		t.Errorf("the result carries %d keys, the Python's BootstrapResult has %d: %v", len(fields), len(wanted), fields)
	}
}

// TestTheResultIsTheBootstrapJSON pins the second reader. /var/lib/atlas/
// bootstrap.json is the on-disk copy the host dashboard and an operator on the
// box read, and it is written from the same map as the result line — so this
// asserts the encoding round-trips rather than that two renderings agree.
func TestTheResultIsTheBootstrapJSON(t *testing.T) {
	result := Result{
		FirecrackerVersion:     "v1.16.0",
		JailerVersion:          "v1.16.0",
		KernelVersion:          "6.8.0-51-generic",
		Architecture:           "x86_64",
		PythonVersion:          "Python 3.14.3",
		VCPUsTotal:             8,
		MemoryMegabytesTotal:   16000,
		PoolDiskGigabytesTotal: 200,
	}

	encoded, err := json.Marshal(result.Fields())
	if err != nil {
		t.Fatalf("encoding the result: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the payload is not JSON: %v", err)
	}
	if decoded["firecracker_version"] != "v1.16.0" {
		t.Errorf("firecracker_version came back as %v", decoded["firecracker_version"])
	}
	if decoded["vcpus_total"] != float64(8) {
		t.Errorf("vcpus_total came back as %v", decoded["vcpus_total"])
	}
	if decoded["python_version"] != "Python 3.14.3" {
		t.Errorf("python_version came back as %v", decoded["python_version"])
	}
}

// TestTheVersionReadingsAreNotInterchangeable. `_binary_version` takes the second
// field ("v1.16.0") and the venv python is reported whole ("Python 3.14.3"). The
// two are one line apart in the Python and one function apart here, and swapping
// them breaks either the firecracker version gate or the e2e that reads the
// python line.
func TestTheVersionReadingsAreNotInterchangeable(t *testing.T) {
	if got := secondField("Firecracker v1.16.0"); got != "v1.16.0" {
		t.Errorf("secondField(firecracker) = %q", got)
	}
	if got := secondField("Python 3.14.3"); got != "3.14.3" {
		t.Errorf("secondField(python) = %q", got)
	}
	if got := secondField("boat"); got != "" {
		t.Errorf("a one-word version line yielded %q, want the empty string", got)
	}
}

// TestTheArchitectureFlagIsAGateNotACorrection. The Task carries the
// architecture Atlas believes the host to be, and bootstrap-server.py's first act
// was to refuse a host that disagreed. Correcting it silently would install the
// other architecture's Firecracker, which fails at the first VM start rather than
// here.
func TestTheArchitectureFlagIsAGateNotACorrection(t *testing.T) {
	if err := checkArchitecture("x86_64", "x86_64"); err != nil {
		t.Errorf("a matching architecture was refused: %v", err)
	}
	if err := checkArchitecture("x86_64", ""); err != nil {
		t.Errorf("an unstated architecture was refused: %v", err)
	}
	err := checkArchitecture("x86_64", "aarch64")
	if err == nil {
		t.Fatal("a host that is not the architecture the Task named was accepted")
	}
	// Both sides in the message: an operator reading a failed Task has to know
	// which one to change.
	for _, named := range []string{"x86_64", "aarch64"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the mismatch does not name %s: %v", named, err)
		}
	}
}

// TestTheDownloadIsTheReleaseTheFlagNamed holds the url and the unpacked paths
// against bootstrap-server.py's `_install_firecracker`. A wrong url is a
// bootstrap that fails minutes in on a 404, and a wrong unpacked path is one that
// downloads correctly and then installs nothing.
func TestTheDownloadIsTheReleaseTheFlagNamed(t *testing.T) {
	download := firecrackerDownload("v1.16.0", "x86_64")

	const url = "https://github.com/firecracker-microvm/firecracker/releases/download/" +
		"v1.16.0/firecracker-v1.16.0-x86_64.tgz"
	if download.url != url {
		t.Errorf("url = %q, want %q", download.url, url)
	}
	wanted := map[string]string{
		"/usr/local/bin/firecracker": "/tmp/firecracker-install/release-v1.16.0-x86_64/firecracker-v1.16.0-x86_64",
		"/usr/local/bin/jailer":      "/tmp/firecracker-install/release-v1.16.0-x86_64/jailer-v1.16.0-x86_64",
	}
	for destination, source := range wanted {
		if download.binaries[destination] != source {
			t.Errorf("%s comes from %q, want %q", destination, download.binaries[destination], source)
		}
	}
	// A second architecture, because the flag exists to be used: nothing about the
	// layout may be pinned to x86_64.
	if arm := firecrackerDownload("v1.16.0", "aarch64"); arm.url == url {
		t.Errorf("the aarch64 release downloads the x86_64 tarball: %s", arm.url)
	}
}
