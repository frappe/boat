package migration

import (
	"context"
	"testing"
)

func TestInjectIdentityThroughClone(t *testing.T) {
	fake := newFakeCommands().exists("sudo dmsetup info " + vmCloneRoot)
	var injected string
	err := InjectIdentity(context.Background(), fake, testUUID, func(_ context.Context, device string) error {
		injected = device
		return nil
	})
	if err != nil {
		t.Fatalf("InjectIdentity: %v", err)
	}
	// The live clone is chosen — the plain LV mounts busy under it.
	if injected != "/dev/mapper/"+vmCloneRoot {
		t.Errorf("injected into %q, want the clone", injected)
	}
	assertTrace(t, fake, "? sudo dmsetup info "+vmCloneRoot)
}

func TestInjectIdentityFallsBackToPlainLV(t *testing.T) {
	fake := newFakeCommands().exists("sudo test -b " + vmDiskDev) // clone gone, plain LV present
	var injected string
	err := InjectIdentity(context.Background(), fake, testUUID, func(_ context.Context, device string) error {
		injected = device
		return nil
	})
	if err != nil {
		t.Fatalf("InjectIdentity: %v", err)
	}
	if injected != vmDiskDev {
		t.Errorf("injected into %q, want the plain LV", injected)
	}
	assertTrace(t, fake,
		"? sudo dmsetup info "+vmCloneRoot,
		"? sudo test -b "+vmDiskDev,
	)
}

func TestInjectIdentityRefusesWhenNeitherPresent(t *testing.T) {
	fake := newFakeCommands() // neither clone nor plain LV
	called := false
	err := InjectIdentity(context.Background(), fake, testUUID, func(context.Context, string) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("InjectIdentity accepted a VM with no device to write through")
	}
	if called {
		t.Error("delegated the write with no device present")
	}
}
