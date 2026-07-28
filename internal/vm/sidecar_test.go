package vm

import (
	"context"
	"strings"
	"testing"
)

func networkEnvironmentText(uid string) string {
	return "# written by provision-vm.py\nTAP_DEVICE=atlas-3f2504e04\n" +
		"ATLAS_NETNS=atlas-3f2504e04f89\nATLAS_FC_UID=" + uid + "\n"
}

func TestFirecrackerUIDComesFromTheVirtualMachinesOwnSidecar(t *testing.T) {
	fake := newFakeCommands()
	files := testFiles(testUUID)
	fake.output("sudo cat "+files.networkEnvironment, networkEnvironmentText("247312"))

	uid, err := newTestManager(fake).FirecrackerUID(context.Background(), nil, testUUID)

	if err != nil {
		t.Fatalf("FirecrackerUID: %v", err)
	}
	if uid != 247312 {
		t.Errorf("uid = %d, want 247312", uid)
	}
	assertTrace(t, fake, "sudo cat "+files.networkEnvironment)
}

// There is no safe default: uid 0 is root, and a snapshot directory or a jail
// node chowned to root is a failure the guest only reveals later — a memory
// snapshot that is never written, a disk Firecracker cannot open. So a sidecar
// that does not name the uid fails the verb here instead.
func TestAMissingFirecrackerUIDIsRefusedRatherThanDefaulted(t *testing.T) {
	for name, text := range map[string]string{
		"no such key":  "TAP_DEVICE=atlas-3f2504e04\n",
		"not a number": networkEnvironmentText("root"),
	} {
		fake := newFakeCommands()
		fake.output("sudo cat "+testFiles(testUUID).networkEnvironment, text)

		uid, err := newTestManager(fake).FirecrackerUID(context.Background(), nil, testUUID)

		if err == nil {
			t.Errorf("%s: FirecrackerUID = %d, want a refusal", name, uid)
		} else if !strings.Contains(err.Error(), "ATLAS_FC_UID") {
			t.Errorf("%s: the failure does not name the key: %v", name, err)
		}
	}
}

// A sidecar that cannot be read at all is a failure, not an empty file: the VM
// tree is 0700 owned by the very uid being read, so "could not read" is exactly
// the case where guessing would be worst.
func TestAnUnreadableSidecarFailsTheRead(t *testing.T) {
	fake := newFakeCommands()
	fake.reply("sudo cat "+testFiles(testUUID).networkEnvironment, false)

	if _, err := newTestManager(fake).FirecrackerUID(context.Background(), nil, testUUID); err == nil {
		t.Error("an unreadable sidecar was read as no uid at all")
	}
}
