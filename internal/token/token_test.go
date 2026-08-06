package token

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A fixed clock so an expiry is crossed by choosing when "now" is, not by
// waiting. The store reads store.now on every Current, so a test moves time by
// reassigning it.
var noon = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

// The WO-0 static token: a bare string, trailing newline and all, that never
// expires. This is the form on every host today, so it has to keep working
// exactly as it did.
func TestABareTokenIsReadWithoutItsNewlineAndNeverExpires(t *testing.T) {
	store, err := Open(write(t, "a-short-lived-token\n"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.now = func() time.Time { return noon.Add(100 * 365 * 24 * time.Hour) }
	if got := store.Current(); got != "a-short-lived-token" {
		t.Fatalf("Current() = %q, want the token without its newline and unexpired", got)
	}
}

// A missing file is no token, not a failure: a socket-only daemon holds none.
func TestAMissingFileIsAnEmptyToken(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := store.Current(); got != "" {
		t.Fatalf("Current() = %q, want empty for a missing file", got)
	}
}

// The JSON form Atlas writes carries a hard expiry. Before it, the token is
// served; at or after it, Current is empty and the listener refuses — fail
// closed, so a token Atlas had time to rotate and did not stops being trusted.
func TestAJSONTokenIsAcceptedBeforeItsExpiryAndRefusedAtIt(t *testing.T) {
	path := write(t, `{"token":"minted-token","hard_expires_at":"2026-08-06T13:00:00Z"}`)
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	store.now = func() time.Time { return noon } // an hour before expiry
	if got := store.Current(); got != "minted-token" {
		t.Fatalf("before expiry Current() = %q, want the minted token", got)
	}

	store.now = func() time.Time { return noon.Add(time.Hour) } // exactly at expiry
	if got := store.Current(); got != "" {
		t.Fatalf("at expiry Current() = %q, want empty", got)
	}

	store.now = func() time.Time { return noon.Add(2 * time.Hour) } // past it
	if got := store.Current(); got != "" {
		t.Fatalf("past expiry Current() = %q, want empty", got)
	}
}

// Rotation reaches a running daemon through Reload: the file is replaced and the
// store re-read, and the next Current is the new token. This is the restart-free
// path Atlas rotates over.
func TestReloadPicksUpARotatedToken(t *testing.T) {
	path := write(t, "first-token")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := store.Current(); got != "first-token" {
		t.Fatalf("Current() = %q, want the first token", got)
	}

	if err := os.WriteFile(path, []byte("second-token"), 0o600); err != nil {
		t.Fatalf("rewrite token: %v", err)
	}
	// Not seen until the reload: the store serves what it loaded, not what the
	// file says this instant, so a half-written rotation is never read mid-write.
	if got := store.Current(); got != "first-token" {
		t.Fatalf("before reload Current() = %q, want the first token still", got)
	}
	if err := store.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := store.Current(); got != "second-token" {
		t.Fatalf("after reload Current() = %q, want the rotated token", got)
	}
}

// A file gone missing clears the token: a rotation that deleted the file, or an
// operator who did, disarms the tunnel rather than leaving the old secret live.
func TestReloadOfADeletedFileClearsTheToken(t *testing.T) {
	path := write(t, "a-token")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove token: %v", err)
	}
	if err := store.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := store.Current(); got != "" {
		t.Fatalf("Current() = %q, want empty after the file was removed", got)
	}
}

// A file that opens with '{' is held to the JSON contract: a malformed rotation
// is refused, not accepted as a literal token no caller could present.
func TestAMalformedJSONTokenIsRefused(t *testing.T) {
	if _, err := Open(write(t, `{"token": "unterminated`)); err == nil {
		t.Fatal("a malformed token document was accepted")
	}
}
