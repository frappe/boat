package snapshot

import (
	"context"
	"testing"

	"github.com/frappe/boat/internal/paths"
)

const (
	warmSnapshotName = "atlas-snap-golden"
	warmSnapshotPath = "/dev/atlas/" + warmSnapshotName
	warmSnapshotDev  = "/dev/atlas/" + warmSnapshotName
	warmMemoryDir    = "/var/lib/atlas/snapshots/snap-golden"
)

// A minimal /proc/cpuinfo: one processor block, then a blank line and a repeat the
// parse must stop at. "fpu vme de" sorts to "de fpu vme", whose SHA-256 prefix is
// the digest below.
const cpuinfoFixture = "processor\t: 0\n" +
	"model name\t: Intel(R) Xeon(R) CPU\n" +
	"microcode\t: 0xf0\n" +
	"flags\t: fpu vme de\n" +
	"\n" +
	"processor\t: 1\n" +
	"model name\t: Intel(R) Xeon(R) CPU\n"

const (
	wantSignatureCompact = `{"cpu_model":"Intel(R) Xeon(R) CPU","microcode":"0xf0",` +
		`"cpu_flags_sha256":"4016e4ce6f37fb56","kernel":"6.8.0-31-generic","firecracker":"v1.16.0"}`
	wantSignatureFile = "{\n" +
		" \"cpu_model\": \"Intel(R) Xeon(R) CPU\",\n" +
		" \"microcode\": \"0xf0\",\n" +
		" \"cpu_flags_sha256\": \"4016e4ce6f37fb56\",\n" +
		" \"kernel\": \"6.8.0-31-generic\",\n" +
		" \"firecracker\": \"v1.16.0\"\n" +
		"}\n"
)

// The whole warm capture, asserted end to end because the ordering IS the verb:
// pause, write the memory pair, thin-snapshot the disk at the SAME paused instant,
// resume, then stage the pair durable and record the host signature. The disk
// snapshot sits between the PUT and the resume — that is what makes the frozen RAM
// and the disk a matched pair.
func TestWarmSnapshotVMCapturesPausedInstant(t *testing.T) {
	vm := vmPaths()
	fake := newFakeCommands().
		exists("sudo test -S "+vm.APISocket()).
		output("sudo lvs --noheadings -o data_percent atlas/pool0", " 42.00").
		output("sudo lvs --noheadings -o metadata_percent atlas/pool0", " 7.00").
		output("sudo jq -r "+guestMemoryQuery+" "+vm.FirecrackerConfig(), "512").
		output("df --output=avail -B1 "+paths.AtlasRoot, "Avail\n999999999999\n").
		exists("sudo test -s "+vm.MemorySnapshotVMState()).
		exists("sudo test -s "+vm.MemorySnapshotMemory()).
		exists("test -b "+warmSnapshotDev).
		output("cat /proc/cpuinfo", cpuinfoFixture).
		output("uname -r", "6.8.0-31-generic\n").
		output("/usr/local/bin/firecracker --version", "Firecracker v1.16.0\n").
		output("sudo stat -c %s "+warmMemoryDir+"/mem.bin", "536870912").
		output("sudo blockdev --getsize64 "+warmSnapshotDev, "10737418240\n")

	result, err := WarmSnapshotVM(context.Background(), fake, WarmSnapshotParams{
		UUID: testUUID, FirecrackerUID: testFirecrackerUID,
		SnapshotRootfsPath: warmSnapshotPath, MemoryDirectory: warmMemoryDir,
	})
	if err != nil {
		t.Fatalf("WarmSnapshotVM: %v", err)
	}
	if result.SizeBytes != 10737418240 || result.MemoryBytes != 536870912 {
		t.Errorf("sizes = %+v", result)
	}
	if result.HostSignature != wantSignatureCompact {
		t.Errorf("host signature:\ngot:  %s\nwant: %s", result.HostSignature, wantSignatureCompact)
	}
	if staged := fake.installedFile[warmMemoryDir+"/host-signature.json"]; staged.content != wantSignatureFile {
		t.Errorf("staged host-signature.json:\ngot:  %q\nwant: %q", staged.content, wantSignatureFile)
	}

	dir, name := vm.APISocketDirectory(), vm.APISocketName()
	assertTrace(t, fake,
		"? sudo test -S "+vm.APISocket(),
		"? sudo lvs --noheadings atlas/atlas-data-"+testUUID,
		"sudo lvs --noheadings -o data_percent atlas/pool0",
		"sudo lvs --noheadings -o metadata_percent atlas/pool0",
		"sudo rm -rf "+vm.MemorySnapshotDirectory(),
		"sudo jq -r "+guestMemoryQuery+" "+vm.FirecrackerConfig(),
		"df --output=avail -B1 "+paths.AtlasRoot,
		"fcapi "+dir+" "+name+" PATCH /vm "+pausedStateBody,
		"install-dir 0700 "+vm.MemorySnapshotDirectory(),
		"sudo chown 247312:247312 "+vm.MemorySnapshotDirectory(),
		"fcapi "+dir+" "+name+" PUT /snapshot/create "+memorySnapshotBody,
		"? sudo test -s "+vm.MemorySnapshotVMState(),
		"? sudo test -s "+vm.MemorySnapshotMemory(),
		"? sudo lvs --noheadings atlas/"+warmSnapshotName,
		"sudo lvcreate -s atlas/atlas-vm-"+testUUID+" -n "+warmSnapshotName,
		"sudo lvchange -ay -K atlas/"+warmSnapshotName,
		"sudo udevadm settle",
		"? test -b "+warmSnapshotDev,
		"fcapi "+dir+" "+name+" PATCH /vm "+resumedStateBody,
		"cat /proc/cpuinfo",
		"uname -r",
		"- /usr/local/bin/firecracker --version",
		"install-dir 0755 "+warmMemoryDir,
		"sudo mv "+vm.MemorySnapshotDirectory()+"/vmstate.bin "+warmMemoryDir+"/vmstate.bin",
		"sudo chown root:root "+warmMemoryDir+"/vmstate.bin",
		"sudo chmod 0644 "+warmMemoryDir+"/vmstate.bin",
		"sudo mv "+vm.MemorySnapshotDirectory()+"/mem.bin "+warmMemoryDir+"/mem.bin",
		"sudo chown root:root "+warmMemoryDir+"/mem.bin",
		"sudo chmod 0644 "+warmMemoryDir+"/mem.bin",
		"install-file 0644 "+warmMemoryDir+"/host-signature.json",
		"sudo rm -rf "+vm.MemorySnapshotDirectory(),
		"sudo stat -c %s "+warmMemoryDir+"/mem.bin",
		"sudo blockdev --getsize64 "+warmSnapshotDev,
	)
}

// A warm capture that fails while paused must still resume the golden VM — a VM
// left frozen is an outage. The disk snapshot fails here (lvcreate errors), and
// the resume is issued regardless before the verb returns the error.
func TestWarmSnapshotVMResumesEvenWhenCaptureFails(t *testing.T) {
	vm := vmPaths()
	dir, name := vm.APISocketDirectory(), vm.APISocketName()
	fake := newFakeCommands().
		exists("sudo test -S "+vm.APISocket()).
		output("sudo lvs --noheadings -o data_percent atlas/pool0", " 42.00").
		output("sudo lvs --noheadings -o metadata_percent atlas/pool0", " 7.00").
		output("sudo jq -r "+guestMemoryQuery+" "+vm.FirecrackerConfig(), "512").
		output("df --output=avail -B1 "+paths.AtlasRoot, "Avail\n999999999999\n").
		exists("sudo test -s " + vm.MemorySnapshotVMState()).
		exists("sudo test -s " + vm.MemorySnapshotMemory()).
		fails("sudo lvcreate -s atlas/atlas-vm-" + testUUID + " -n " + warmSnapshotName)

	if _, err := WarmSnapshotVM(context.Background(), fake, WarmSnapshotParams{
		UUID: testUUID, FirecrackerUID: testFirecrackerUID,
		SnapshotRootfsPath: warmSnapshotPath, MemoryDirectory: warmMemoryDir,
	}); err == nil {
		t.Fatal("WarmSnapshotVM hid a capture failure")
	}
	// The resume must have been attempted despite the failure.
	assertIssued(t, fake, "fcapi "+dir+" "+name+" PATCH /vm "+resumedStateBody)
	// And nothing durable was staged.
	assertNotIssued(t, fake, "install-dir 0755")
}

// A VM with a data disk is rejected before anything is paused — the frozen RAM
// references only the root disk, so a data disk would make an unrestorable pair.
func TestWarmSnapshotVMRejectsDataDisk(t *testing.T) {
	vm := vmPaths()
	fake := newFakeCommands().
		exists("sudo test -S " + vm.APISocket()).
		exists("sudo lvs --noheadings atlas/atlas-data-" + testUUID)

	if _, err := WarmSnapshotVM(context.Background(), fake, WarmSnapshotParams{
		UUID: testUUID, FirecrackerUID: testFirecrackerUID,
		SnapshotRootfsPath: warmSnapshotPath, MemoryDirectory: warmMemoryDir,
	}); err == nil {
		t.Fatal("WarmSnapshotVM accepted a VM with a data disk")
	}
	assertNotIssued(t, fake, "fcapi")
}

// A memory directory outside the snapshots tree is refused before any host poke.
func TestWarmSnapshotVMRefusesMemoryDirectoryOutsideTree(t *testing.T) {
	fake := newFakeCommands()
	if _, err := WarmSnapshotVM(context.Background(), fake, WarmSnapshotParams{
		UUID: testUUID, FirecrackerUID: testFirecrackerUID,
		SnapshotRootfsPath: warmSnapshotPath, MemoryDirectory: "/tmp/elsewhere",
	}); err == nil {
		t.Fatal("WarmSnapshotVM accepted a memory directory outside the snapshots tree")
	}
	if len(fake.trace) != 0 {
		t.Errorf("touched the host before the guard: %v", fake.trace)
	}
}
