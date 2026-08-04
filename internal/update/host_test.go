package update

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The run seam is exercised through install_test.go's recorder, unchanged: it
// already records rendered command lines and can be told to fail one, which is
// exactly what RestartAndReattach and Healthy need to script "this unit would not
// come up." Reusing it keeps one fake for the one subprocess seam.

// fakeHealth answers /health with a canned status (or a transport error) and no
// socket under it, and remembers what it was asked so a test can assert the
// probe's URL and method.
type fakeHealth struct {
	status  int
	err     error
	calls   int
	lastURL string
	method  string
}

func (health *fakeHealth) Do(request *http.Request) (*http.Response, error) {
	health.calls++
	health.lastURL = request.URL.String()
	health.method = request.Method
	if health.err != nil {
		return nil, health.err
	}
	return &http.Response{
		StatusCode: health.status,
		Body:       io.NopCloser(strings.NewReader(`{"status":"ok"}`)),
	}, nil
}

// fakeQuiescer records the daemon-side pause/undo in order and can fail either
// call, standing in for the real reconcile+journal implementation the parent will
// wire.
type fakeQuiescer struct {
	steps      []string
	quiesceErr error
	resumeErr  error
}

func (quiescer *fakeQuiescer) Quiesce(context.Context) error {
	quiescer.steps = append(quiescer.steps, "quiesce")
	return quiescer.quiesceErr
}

func (quiescer *fakeQuiescer) Resume(context.Context) error {
	quiescer.steps = append(quiescer.steps, "resume")
	return quiescer.resumeErr
}

func TestRestartAndReattachRestartsTheUnitsInOrder(t *testing.T) {
	runner := newRecorder()
	host := NewHost(nil, runner, nil, []string{networkdUnit, daemonUnit})
	if err := host.RestartAndReattach(context.Background()); err != nil {
		t.Fatalf("RestartAndReattach of healthy units failed: %v", err)
	}
	assertTrace(t, runner, []string{
		"sudo systemctl restart boat-networkd.service",
		"sudo systemctl restart boat.service",
	})
}

// The first unit that will not restart aborts the rest, so apply.go rolls the
// host back to N-1 rather than leaving it half-swapped.
func TestRestartAndReattachStopsAtTheFirstFailure(t *testing.T) {
	runner := newRecorder()
	runner.failing["sudo systemctl restart boat-networkd.service"] = true
	host := NewHost(nil, runner, nil, []string{networkdUnit, daemonUnit})
	if err := host.RestartAndReattach(context.Background()); err == nil {
		t.Fatal("RestartAndReattach hid a unit that would not start")
	}
	if issued(runner, "restart boat.service") {
		t.Error("RestartAndReattach kept going after a unit failed to restart")
	}
}

// NewHost with no units restarts the production default set rather than nothing —
// a Host that swapped the binary and restarted nothing would report success while
// the old code kept serving.
func TestNewHostRestartsTheDefaultUnitsWhenGivenNone(t *testing.T) {
	runner := newRecorder()
	host := NewHost(nil, runner, nil, nil)
	if err := host.RestartAndReattach(context.Background()); err != nil {
		t.Fatalf("RestartAndReattach: %v", err)
	}
	for _, unit := range DefaultUnits() {
		if !issued(runner, "restart "+unit) {
			t.Errorf("default restart set never touched %s", unit)
		}
	}
}

func TestHealthyPassesWhenTheSocketAnswersAndTheUnitsAreActive(t *testing.T) {
	runner := newRecorder()
	health := &fakeHealth{status: http.StatusOK}
	host := NewHost(nil, runner, health, []string{daemonUnit})
	if err := host.Healthy(context.Background()); err != nil {
		t.Fatalf("a healthy swap was reported unhealthy: %v", err)
	}
	if health.calls != 1 || health.method != http.MethodGet || health.lastURL != healthURL {
		t.Errorf("expected one GET %s, got %d call(s) to %s %s", healthURL, health.calls, health.method, health.lastURL)
	}
	if !issued(runner, "systemctl is-active boat.service") {
		t.Error("Healthy never checked unit liveness")
	}
}

// A binary that came up but does not serve /health is unhealthy, and the unit
// liveness check is not even reached — the socket probe is the primary signal.
func TestHealthyFailsAndSkipsUnitsWhenTheSocketDoesNotAnswer(t *testing.T) {
	for name, health := range map[string]*fakeHealth{
		"transport error": {err: errors.New("connection refused")},
		"non-200":         {status: http.StatusInternalServerError},
	} {
		runner := newRecorder()
		host := NewHost(nil, runner, health, []string{daemonUnit})
		if err := host.Healthy(context.Background()); err == nil {
			t.Errorf("%s: an unserving binary was reported healthy", name)
		}
		if len(runner.trace) != 0 {
			t.Errorf("%s: unit liveness was checked despite a dead socket: %v", name, runner.trace)
		}
	}
}

// A binary that answers /health but whose unit crash-looped afterwards is
// unhealthy too — is-active is the second, independent check.
func TestHealthyFailsWhenAUnitIsNotActive(t *testing.T) {
	runner := newRecorder()
	runner.failing["systemctl is-active boat.service"] = true
	health := &fakeHealth{status: http.StatusOK}
	host := NewHost(nil, runner, health, []string{daemonUnit})
	if err := host.Healthy(context.Background()); err == nil {
		t.Fatal("a crash-looped unit was reported healthy")
	}
}

func TestQuiesceAndResumeDelegateToTheQuiescer(t *testing.T) {
	quiescer := &fakeQuiescer{}
	host := NewHost(quiescer, newRecorder(), nil, []string{daemonUnit})
	if err := host.Quiesce(context.Background()); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if err := host.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	assertOrder(t, quiescer.steps, []string{"quiesce", "resume"})
}

// A Quiescer that refuses is a reason to abort the update, so its error must reach
// apply.go rather than be swallowed.
func TestQuiesceSurfacesTheQuiescerError(t *testing.T) {
	quiescer := &fakeQuiescer{quiesceErr: errors.New("in-flight op would not checkpoint")}
	host := NewHost(quiescer, newRecorder(), nil, []string{daemonUnit})
	err := host.Quiesce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "in-flight op would not checkpoint") {
		t.Fatalf("Quiesce hid the quiescer's failure: %v", err)
	}
}

// The concrete DaemonHost must plug into apply.go's orchestration: a good release
// runs verify → quiesce → install → restart → health with no rollback and no
// resume, and every host effect lands on the injected seams.
func TestApplyDrivesTheConcreteHost(t *testing.T) {
	installCommands := newRecorder()
	runner := newRecorder()
	quiescer := &fakeQuiescer{}
	health := &fakeHealth{status: http.StatusOK}
	host := NewHost(quiescer, runner, health, []string{daemonUnit})

	key, secret := testKey(t)
	release := signedRelease(secret, "v2", []byte("new boat"))
	if err := Apply(context.Background(), installCommands, host, release, key); err != nil {
		t.Fatalf("Apply through the concrete host failed: %v", err)
	}

	// The daemon was quiesced and never resumed (resume is the abort path only).
	assertOrder(t, quiescer.steps, []string{"quiesce"})
	if count(quiescer.steps, "resume") != 0 {
		t.Errorf("a successful update still resumed the daemon: %v", quiescer.steps)
	}
	// The binary was swapped, the unit restarted onto it, and it was health-checked.
	if !issued(installCommands, "mv -f /usr/local/bin/boat.staging") {
		t.Error("Apply never swapped the binary in")
	}
	if !issued(runner, "sudo systemctl restart boat.service") {
		t.Error("Apply never restarted the daemon onto the new binary")
	}
	if health.calls != 1 {
		t.Errorf("the swapped binary was health-probed %d times, want 1", health.calls)
	}
	// No rollback: N-1 was never restored.
	if issued(installCommands, "boat.previous /usr/local/bin/boat") {
		t.Error("a healthy update still rolled back to N-1")
	}
}
