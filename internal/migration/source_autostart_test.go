package migration

import (
	"context"
	"testing"
)

// enabled=false is what Pending sends: plain `disable`, which removes the unit's
// WantedBy=multi-user.target symlink so a source-host reboot cannot cold-boot a
// second copy. NOT `disable --now` — Cleanup's teardown is the one that stops the
// guest; this phase leaves a running source running so the rollback can restart it.
func TestSourceAutostartDisableRemovesRebootSurvival(t *testing.T) {
	fake := newFakeCommands()
	if err := SourceAutostart(context.Background(), fake, testUUID, false); err != nil {
		t.Fatalf("SourceAutostart(disable): %v", err)
	}
	assertTrace(t, fake, "sudo systemctl disable firecracker-vm@"+testUUID+".service")
}

// enabled=true is the operator's inverse: put the unit back in multi-user.target
// so an abandoned source copy survives a reboot again.
func TestSourceAutostartEnableRestoresRebootSurvival(t *testing.T) {
	fake := newFakeCommands()
	if err := SourceAutostart(context.Background(), fake, testUUID, true); err != nil {
		t.Fatalf("SourceAutostart(enable): %v", err)
	}
	assertTrace(t, fake, "sudo systemctl enable firecracker-vm@"+testUUID+".service")
}
