package api

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"github.com/frappe/boat/internal/update"
	"github.com/frappe/boat/internal/wire"
)

// testUpdateKey is a fixed ed25519 pair, so the handler tests are reproducible with
// no entropy and can sign a release the configured key will verify.
func testUpdateKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index + 7)
	}
	private := ed25519.NewKeyFromSeed(seed)
	return private.Public().(ed25519.PublicKey), private
}

// A host Atlas has not enrolled in self-update holds no key, and says so with a 503
// an operator can act on — not a 400 that would read as "your release is bad".
func TestUpdateRefusesWhenNoTrustedKeyConfigured(t *testing.T) {
	server := newTestServer(newFakeStore(), &fakeVirtualMachines{})
	response, _ := server.Update(context.Background(), wire.UpdateRequestObject{
		Body: &wire.UpdateRequest{Version: "v2", Sha256: "00", Binary: []byte("x")},
	})
	assertErrorStatus(t, response, 503)
}

// A release the trusted key did not sign is refused with a 400, and — crucially —
// nothing is staged and no updater is launched.
func TestUpdateRefusesAReleaseTheKeyDidNotSign(t *testing.T) {
	public, _ := testUpdateKey(t)
	server := newTestServer(newFakeStore(), &fakeVirtualMachines{})
	server.updateKey = public
	server.stateDirectory = t.TempDir()
	launched := false
	server.launchUpdater = func(string, string) error { launched = true; return nil }

	response, _ := server.Update(context.Background(), wire.UpdateRequestObject{
		Body: &wire.UpdateRequest{Version: "v2", Sha256: "abcd", Binary: []byte("x"), Signature: []byte("forged")},
	})
	assertErrorStatus(t, response, 400)
	if launched {
		t.Error("an unverified release launched the updater")
	}
	if entries, _ := os.ReadDir(filepath.Join(server.stateDirectory, "update")); len(entries) != 0 {
		t.Error("an unverified release was staged to disk")
	}
}

// A verified release is staged where the updater will read it, and the detached
// updater is launched; the handler answers 202 with the id and version.
func TestUpdateStagesAndLaunchesAVerifiedRelease(t *testing.T) {
	public, private := testUpdateKey(t)
	server := newTestServer(newFakeStore(), &fakeVirtualMachines{})
	server.updateKey = public
	server.stateDirectory = t.TempDir()
	launchedDir := ""
	server.launchUpdater = func(id, dir string) error { launchedDir = dir; return nil }

	release := update.SignRelease(private, "v2", []byte("a new boat binary"))
	response, err := server.Update(context.Background(), wire.UpdateRequestObject{
		Body: &wire.UpdateRequest{
			Version:   release.Manifest.Version,
			Sha256:    release.Manifest.SHA256,
			Binary:    release.Binary,
			Signature: release.Signature,
		},
	})
	if err != nil {
		t.Fatalf("Update returned a transport error: %v", err)
	}
	accepted, ok := response.(wire.Update202JSONResponse)
	if !ok {
		t.Fatalf("want a 202 Update accepted, got %T", response)
	}
	if accepted.Version != "v2" {
		t.Errorf("accepted version = %q, want v2", accepted.Version)
	}
	if launchedDir == "" {
		t.Fatal("the detached updater was never launched")
	}
	// The updater reads a real binary back from where the handler staged it.
	staged, err := update.ReadStaged(launchedDir)
	if err != nil {
		t.Fatalf("the staged release could not be read back: %v", err)
	}
	if got := string(staged.Binary); got != "a new boat binary" {
		t.Errorf("staged binary = %q, want the release bytes", got)
	}
}

// assertErrorStatus asserts an error response with the given HTTP status.
func assertErrorStatus(t *testing.T, response any, want int) {
	t.Helper()
	failure, ok := response.(*errorResponse)
	if !ok {
		t.Fatalf("want an *errorResponse, got %T", response)
	}
	if failure.statusCode != want {
		t.Errorf("status = %d, want %d", failure.statusCode, want)
	}
}
