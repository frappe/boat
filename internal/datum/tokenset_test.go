package datum

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenSetReadsHostAndVMTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, []byte(`{"host":"H","vms":{"uuid-a":"A","uuid-b":"B"}}`), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	set, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := set.HostToken(); got != "H" {
		t.Errorf("HostToken() = %q, want %q", got, "H")
	}
	if got := set.TokenFor("uuid-a"); got != "A" {
		t.Errorf(`TokenFor("uuid-a") = %q, want %q`, got, "A")
	}
	if got := set.TokenFor("uuid-b"); got != "B" {
		t.Errorf(`TokenFor("uuid-b") = %q, want %q`, got, "B")
	}
	if got := set.TokenFor("nope"); got != "" {
		t.Errorf(`TokenFor("nope") = %q, want ""`, got)
	}
}

func TestTokenSetMissingFileIsEmpty(t *testing.T) {
	set, err := Open(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := set.HostToken(); got != "" {
		t.Errorf("HostToken() = %q, want empty", got)
	}
}

func TestTokenSetReloadPicksUpChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, []byte(`{"host":"H1"}`), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	set, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"host":"H2"}`), 0o600); err != nil {
		t.Fatalf("rewrite token file: %v", err)
	}
	if err := set.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := set.HostToken(); got != "H2" {
		t.Errorf("after rewrite HostToken() = %q, want %q", got, "H2")
	}
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatalf("truncate token file: %v", err)
	}
	if err := set.Reload(); err != nil {
		t.Fatalf("Reload after truncate: %v", err)
	}
	if got := set.HostToken(); got != "" {
		t.Errorf("after truncate HostToken() = %q, want empty", got)
	}
}

func TestTokenSetMalformedIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, []byte("not json {"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open: got nil error, want non-nil for a malformed file")
	}
}
