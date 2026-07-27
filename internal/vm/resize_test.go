package vm

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const (
	// What jq hands back, and therefore what is installed over the config.
	resizedConfig = `{"machine-config":{"vcpu_count":4,"mem_size_mib":8192}}`

	// A launcher as provision generates it: the cgroup values on their own
	// continued lines, contiguous, surrounded by flags this rewrite must leave
	// byte for byte alone.
	testLauncher = "#!/bin/sh\n" +
		"exec /usr/bin/jailer \\\n" +
		"    --id " + testUUID + " \\\n" +
		"    --cgroup memory.max=4294967296 \\\n" +
		"    --cgroup cpu.max=100000 100000 \\\n" +
		"    --chroot-base-dir /var/lib/atlas/virtual-machines/" + testUUID + "/jail\n"
)

type resizeCommandSet struct {
	configPresent string
	rootExists    string
	rootSize      string
	dataExists    string
	dataSize      string
	dropSnapshot  string
	rewriteConfig string
	installConfig string
	ownConfig     string
	moveConfig    string
	growRoot      string
	growData      string
	growDataRaw   string
	readLauncher  string
}

func resizeCommands(request ResizeRequest) resizeCommandSet {
	files := testFiles(testUUID)
	rootDevice := "/dev/atlas/atlas-vm-" + testUUID
	dataDevice := "/dev/atlas/atlas-data-" + testUUID
	return resizeCommandSet{
		configPresent: "sudo test -f " + files.firecrackerConfig,
		rootExists:    "sudo lvs --noheadings atlas/atlas-vm-" + testUUID,
		rootSize:      "sudo blockdev --getsize64 " + rootDevice,
		dataExists:    "sudo lvs --noheadings atlas/atlas-data-" + testUUID,
		dataSize:      "sudo blockdev --getsize64 " + dataDevice,
		dropSnapshot:  "sudo rm -rf " + files.memorySnapshotDirectory,
		rewriteConfig: fmt.Sprintf("sudo jq --argjson vcpus %d --argjson mem %d %s %s",
			request.VCPUs, request.MemoryMB, machineConfigFilter, files.firecrackerConfig),
		installConfig: fmt.Sprintf("install -m 0644 %q %s.new", resizedConfig, files.firecrackerConfig),
		ownConfig: "sudo chown --reference=" + files.firecrackerConfig + " " +
			files.firecrackerConfig + ".new",
		moveConfig:   "sudo mv " + files.firecrackerConfig + ".new " + files.firecrackerConfig,
		growRoot:     fmt.Sprintf("sudo lvextend -r -L %dG %s", request.DiskGB, rootDevice),
		growData:     fmt.Sprintf("sudo lvextend -r -L %dG %s", request.DataDiskGB, dataDevice),
		growDataRaw:  fmt.Sprintf("sudo lvextend -L %dG %s", request.DataDiskGB, dataDevice),
		readLauncher: "sudo cat " + files.jailerLaunch,
	}
}

// aTwentyGibibyteRootDisk is the size the shrink guard reads back.
func aTwentyGibibyteRootDisk(fake *fakeCommands, commands resizeCommandSet) {
	fake.output(commands.rootSize, "21474836480\n")
	fake.output(commands.rewriteConfig, resizedConfig)
}

func TestResizeRewritesTheMachineConfigAndGrowsTheRootDisk(t *testing.T) {
	request := ResizeRequest{VCPUs: 4, MemoryMB: 8192, DiskGB: 40}
	commands := resizeCommands(request)
	fake := newFakeCommands()
	aTwentyGibibyteRootDisk(fake, commands)

	if err := newTestManager(fake).Resize(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	assertTrace(t, fake,
		"? "+commands.configPresent,
		"? "+commands.rootExists,
		commands.rootSize,
		commands.dropSnapshot,
		commands.rewriteConfig,
		commands.installConfig,
		commands.ownConfig,
		commands.moveConfig,
		"- "+commands.growRoot,
	)
}

// A disk only ever grows. Shrinking the volume under a filesystem that has
// written past the new boundary destroys whatever lives there — and lvextend's
// own refusal is not the guard, because its exit code is discarded.
func TestResizeRefusesToShrinkTheRootDisk(t *testing.T) {
	request := ResizeRequest{VCPUs: 4, MemoryMB: 8192, DiskGB: 10}
	commands := resizeCommands(request)
	fake := newFakeCommands()
	aTwentyGibibyteRootDisk(fake, commands)

	err := newTestManager(fake).Resize(context.Background(), nil, testUUID, request)

	if err == nil {
		t.Fatal("Resize succeeded, want a shrink refused")
	}
	if !strings.Contains(err.Error(), "only grows") {
		t.Errorf("the refusal must say why: %v", err)
	}
	// Nothing was written: the refusal happens before the first mutation, so a
	// declined resize leaves the VM exactly as it was.
	assertTrace(t, fake,
		"? "+commands.configPresent,
		"? "+commands.rootExists,
		commands.rootSize,
	)
}

func TestResizeRefusesToShrinkTheDataDisk(t *testing.T) {
	request := ResizeRequest{VCPUs: 4, MemoryMB: 8192, DiskGB: 40, DataDiskGB: 10}
	commands := resizeCommands(request)
	fake := newFakeCommands()
	aTwentyGibibyteRootDisk(fake, commands)
	fake.output(commands.dataSize, "107374182400\n")

	err := newTestManager(fake).Resize(context.Background(), nil, testUUID, request)

	if err == nil {
		t.Fatal("Resize succeeded, want the data disk's shrink refused")
	}
	assertTrace(t, fake,
		"? "+commands.configPresent,
		"? "+commands.rootExists,
		commands.rootSize,
		"? "+commands.dataExists,
		commands.dataSize,
	)
}

func TestResizeGrowsAFormattedDataDiskWithItsFilesystem(t *testing.T) {
	request := ResizeRequest{VCPUs: 4, MemoryMB: 8192, DiskGB: 40, DataDiskGB: 100, DataDiskFormatted: true}
	commands := resizeCommands(request)
	fake := newFakeCommands()
	aTwentyGibibyteRootDisk(fake, commands)
	fake.output(commands.dataSize, "21474836480\n")

	if err := newTestManager(fake).Resize(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if fake.trace[len(fake.trace)-1] != "- "+commands.growData {
		t.Errorf("last command was %q, want the data disk grown with -r", fake.trace[len(fake.trace)-1])
	}
}

// A raw attached disk has no filesystem to resize, so the volume grows alone.
func TestResizeGrowsARawDataDiskWithoutTouchingAFilesystem(t *testing.T) {
	request := ResizeRequest{VCPUs: 4, MemoryMB: 8192, DiskGB: 40, DataDiskGB: 100}
	commands := resizeCommands(request)
	fake := newFakeCommands()
	aTwentyGibibyteRootDisk(fake, commands)
	fake.output(commands.dataSize, "21474836480\n")

	if err := newTestManager(fake).Resize(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if fake.trace[len(fake.trace)-1] != "- "+commands.growDataRaw {
		t.Errorf("last command was %q, want a plain lvextend", fake.trace[len(fake.trace)-1])
	}
}

// No size given is no disk touched, which is what a resize of vCPU and memory
// alone asks for — and the reason the zero value cannot be read as "0 GiB".
func TestResizeWithNoDiskSizeLeavesTheDisksAlone(t *testing.T) {
	request := ResizeRequest{VCPUs: 4, MemoryMB: 8192}
	commands := resizeCommands(request)
	fake := newFakeCommands()
	aTwentyGibibyteRootDisk(fake, commands)

	if err := newTestManager(fake).Resize(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	for _, line := range fake.trace {
		if strings.Contains(line, "lvextend") {
			t.Errorf("a sizeless resize grew a volume: %s", line)
		}
	}
}

func TestResizeRefusesAVirtualMachineWithNoConfig(t *testing.T) {
	request := ResizeRequest{VCPUs: 4, MemoryMB: 8192, DiskGB: 40}
	commands := resizeCommands(request)
	fake := newFakeCommands()
	fake.reply(commands.configPresent, false)

	if err := newTestManager(fake).Resize(context.Background(), nil, testUUID, request); err == nil {
		t.Fatal("Resize succeeded with no firecracker config, want a refusal")
	}
	assertTrace(t, fake, "? "+commands.configPresent)
}

// The launcher's memory.max is a SECOND ceiling, independent of the guest RAM
// in firecracker.json. Leaving it stale caps the guest below the RAM it was
// just given, and the kernel OOM-kills Firecracker on the next boot.
func TestResizeRewritesTheLauncherCgroups(t *testing.T) {
	request := ResizeRequest{
		VCPUs: 4, MemoryMB: 8192, DiskGB: 40,
		CgroupArguments: []string{"memory.max=8589934592", "cpu.max=400000 100000"},
	}
	commands := resizeCommands(request)
	files := testFiles(testUUID)
	fake := newFakeCommands()
	aTwentyGibibyteRootDisk(fake, commands)
	fake.output(commands.readLauncher, testLauncher)

	if err := newTestManager(fake).Resize(context.Background(), nil, testUUID, request); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	rewritten := strings.Replace(
		strings.Replace(testLauncher,
			"    --cgroup memory.max=4294967296 \\\n", "    --cgroup memory.max=8589934592 \\\n", 1),
		"    --cgroup cpu.max=100000 100000 \\\n", "    --cgroup 'cpu.max=400000 100000' \\\n", 1)
	assertTrace(t, fake,
		"? "+commands.configPresent,
		"? "+commands.rootExists,
		commands.rootSize,
		commands.dropSnapshot,
		commands.rewriteConfig,
		commands.installConfig,
		commands.ownConfig,
		commands.moveConfig,
		commands.readLauncher,
		fmt.Sprintf("install -m 0755 %q %s.new", rewritten, files.jailerLaunch),
		"sudo chown --reference="+files.jailerLaunch+" "+files.jailerLaunch+".new",
		"sudo mv "+files.jailerLaunch+".new "+files.jailerLaunch,
		"- "+commands.growRoot,
	)
}

// A launcher this rewrite does not recognise is a launcher whose caps would
// silently stay stale — which is the OOM-kill the rewrite exists to prevent.
func TestSpliceCgroupArgumentsRefusesALauncherItDoesNotUnderstand(t *testing.T) {
	for name, launcher := range map[string]string{
		"no cgroup lines": "#!/bin/sh\nexec /usr/bin/jailer --id x\n",
		"not contiguous": "exec jailer \\\n    --cgroup memory.max=1 \\\n" +
			"    --netns /var/run/netns/x \\\n    --cgroup cpu.max=1 100 \\\n",
	} {
		if _, err := spliceCgroupArguments(launcher, []string{"memory.max=2"}); err == nil {
			t.Errorf("%s: splice succeeded, want a refusal", name)
		}
	}
}

func TestSpliceCgroupArgumentsKeepsEveryOtherLine(t *testing.T) {
	spliced, err := spliceCgroupArguments(testLauncher, []string{"memory.max=1"})
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	for _, line := range []string{"#!/bin/sh", "    --id " + testUUID + " \\", "    --chroot-base-dir"} {
		if !strings.Contains(spliced, line) {
			t.Errorf("splice dropped %q:\n%s", line, spliced)
		}
	}
	if strings.Count(spliced, "--cgroup") != 1 {
		t.Errorf("want exactly the one new cgroup line:\n%s", spliced)
	}
}
