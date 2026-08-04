package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
)

// fakeHost records the daemon-side steps in order and can be told to fail one of
// them, so a test scripts "restart failed" or "unhealthy" and asserts the recovery.
type fakeHost struct {
	steps  []string
	failAt map[string]error
}

func newFakeHost() *fakeHost { return &fakeHost{failAt: map[string]error{}} }

// do records the step and fires a scripted failure at most ONCE, so a test that
// fails the new binary's restart still lets the known-good N-1 restart succeed —
// modelling "the new binary would not come up, the old one does".
func (h *fakeHost) do(name string) error {
	h.steps = append(h.steps, name)
	if err, ok := h.failAt[name]; ok {
		delete(h.failAt, name)
		return err
	}
	return nil
}
func (h *fakeHost) Quiesce(context.Context) error            { return h.do("quiesce") }
func (h *fakeHost) Resume(context.Context) error             { return h.do("resume") }
func (h *fakeHost) RestartAndReattach(context.Context) error { return h.do("restart") }
func (h *fakeHost) Healthy(context.Context) error            { return h.do("healthy") }

// applyFixture returns a recorder, a fakeHost, a genuine signed release and the key
// that vouches for it — the happy-path inputs every case starts from.
func applyFixture(t *testing.T) (*recorder, *fakeHost, Release, ed25519.PublicKey) {
	t.Helper()
	public, private := testKey(t)
	return newRecorder(), newFakeHost(), signedRelease(private, "v2", []byte("new boat")), public
}

func TestApplyRunsTheSevenStepsInOrder(t *testing.T) {
	cmd, host, release, key := applyFixture(t)
	if err := Apply(context.Background(), cmd, host, release, key); err != nil {
		t.Fatalf("Apply of a good release failed: %v", err)
	}
	// Quiesce before the swap, restart after it, then the health check.
	assertOrder(t, host.steps, []string{"quiesce", "restart", "healthy"})
	if !issued(cmd, "mv -f /usr/local/bin/boat.staging") {
		t.Error("Apply never swapped the binary in")
	}
	// The swap must land AFTER quiesce and BEFORE restart.
	if host.steps[0] != "quiesce" {
		t.Errorf("first host step was %q, want quiesce", host.steps[0])
	}
}

// A release the trusted key did not sign never touches the host at all.
func TestApplyRefusesAnUnverifiedReleaseBeforeAnyEffect(t *testing.T) {
	cmd, host, release, key := applyFixture(t)
	release.Binary = []byte("tampered") // breaks the checksum
	err := Apply(context.Background(), cmd, host, release, key)
	if err == nil {
		t.Fatal("Apply accepted a tampered release")
	}
	if len(host.steps) != 0 || len(cmd.trace) != 0 {
		t.Errorf("a rejected release still touched the host: host=%v cmd=%v", host.steps, cmd.trace)
	}
}

// A restart that fails after the swap must roll back to N-1 and restart onto it.
func TestApplyRollsBackWhenRestartFails(t *testing.T) {
	cmd, host, release, key := applyFixture(t)
	host.failAt["restart"] = errors.New("unit would not start")
	err := Apply(context.Background(), cmd, host, release, key)
	if err == nil || !strings.Contains(err.Error(), "rolled back to N-1") {
		t.Fatalf("expected a rollback error, got %v", err)
	}
	if !issued(cmd, "mv -f /usr/local/bin/boat.previous /usr/local/bin/boat") {
		t.Error("rollback never restored N-1")
	}
	// restart is attempted twice: once for the new binary (fails), once onto N-1.
	if count(host.steps, "restart") != 2 {
		t.Errorf("restart ran %d times, want 2 (new + rollback)", count(host.steps, "restart"))
	}
}

// A binary that swaps and restarts but fails its health check is rolled back too.
func TestApplyRollsBackWhenUnhealthy(t *testing.T) {
	cmd, host, release, key := applyFixture(t)
	host.failAt["healthy"] = errors.New("export did not answer")
	err := Apply(context.Background(), cmd, host, release, key)
	if err == nil || !strings.Contains(err.Error(), "rolled back to N-1") {
		t.Fatalf("expected a rollback error, got %v", err)
	}
	if !issued(cmd, "mv -f /usr/local/bin/boat.previous /usr/local/bin/boat") {
		t.Error("an unhealthy swap was not rolled back")
	}
}

// A staging failure is BEFORE the swap: resume, do not roll back a binary that was
// never replaced.
func TestApplyResumesWithoutRollbackWhenInstallFails(t *testing.T) {
	cmd, host, release, key := applyFixture(t)
	cmd.failing["install -m 0755 /usr/local/bin/boat.staging (8 bytes)"] = true
	err := Apply(context.Background(), cmd, host, release, key)
	if err == nil {
		t.Fatal("Apply hid an install failure")
	}
	if issued(cmd, "mv -f /usr/local/bin/boat.previous") {
		t.Error("Apply rolled back after a failure that never swapped")
	}
	if count(host.steps, "resume") != 1 {
		t.Errorf("the daemon was not resumed after an aborted install: %v", host.steps)
	}
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	next := 0
	for _, step := range got {
		if next < len(want) && step == want[next] {
			next++
		}
	}
	if next != len(want) {
		t.Errorf("steps %v did not contain %v in order", got, want)
	}
}

func count(steps []string, name string) int {
	n := 0
	for _, step := range steps {
		if step == name {
			n++
		}
	}
	return n
}
