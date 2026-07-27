package vm

import (
	"context"
	"strings"
	"testing"
)

// The two lines a pause or a resume is made of. Note what is NOT here: no
// systemctl. Pause goes through the Firecracker API precisely so the unit stays
// active and the guest keeps its RAM, and a pause that reached for systemd
// would be a stop wearing the wrong name.
func pauseCommands(body string) (socket, patch string) {
	files := testFiles(testUUID)
	return "sudo test -S " + files.apiSocket,
		"firecracker-api PATCH /vm socket=" + files.apiSocketDirectory +
			"/firecracker.socket body=" + body
}

func TestPauseFreezesTheGuestThroughTheFirecrackerAPI(t *testing.T) {
	socket, patch := pauseCommands(pauseStateBody)
	fake := newFakeCommands()

	if err := newTestManager(fake).Pause(context.Background(), nil, testUUID); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	assertTrace(t, fake, "? "+socket, patch)
}

func TestResumeUnfreezesTheGuestThroughTheFirecrackerAPI(t *testing.T) {
	socket, patch := pauseCommands(resumeStateBody)
	fake := newFakeCommands()

	if err := newTestManager(fake).Resume(context.Background(), nil, testUUID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	assertTrace(t, fake, "? "+socket, patch)
}

// A paused VM still holds its RAM because its unit is still up. If either verb
// ever grew a systemctl line, that property would be gone and nothing else in
// the package would notice.
func TestPauseAndResumeNeverTouchSystemd(t *testing.T) {
	for _, pause := range []bool{true, false} {
		fake := newFakeCommands()
		manager := newTestManager(fake)
		var err error
		if pause {
			err = manager.Pause(context.Background(), nil, testUUID)
		} else {
			err = manager.Resume(context.Background(), nil, testUUID)
		}
		if err != nil {
			t.Fatalf("pause=%v: %v", pause, err)
		}
		for _, line := range fake.trace {
			if strings.Contains(line, "systemctl") {
				t.Errorf("pause=%v reached for systemd: %s", pause, line)
			}
		}
	}
}

// No socket means no running Firecracker. Saying so beats a connection error
// from curl, which names the socket and nothing about the VM.
func TestPauseRefusesWhenTheSocketIsGone(t *testing.T) {
	socket, _ := pauseCommands(pauseStateBody)
	fake := newFakeCommands()
	fake.reply(socket, false)

	err := newTestManager(fake).Pause(context.Background(), nil, testUUID)

	if err == nil {
		t.Fatal("Pause succeeded, want a refusal: there is no socket to pause through")
	}
	assertTrace(t, fake, "? "+socket)
}

func TestResumeReportsAStateChangeFirecrackerRefused(t *testing.T) {
	socket, patch := pauseCommands(resumeStateBody)
	fake := newFakeCommands()
	fake.reply(patch, false)

	if err := newTestManager(fake).Resume(context.Background(), nil, testUUID); err == nil {
		t.Fatal("Resume succeeded, want the refused API call reported")
	}
	assertTrace(t, fake, "? "+socket, patch)
}
