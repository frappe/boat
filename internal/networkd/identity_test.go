package networkd

import (
	"os"
	"path/filepath"
	"testing"
)

func writeIdentity(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "identity.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadIdentityReadsTheThreeFields(t *testing.T) {
	path := writeIdentity(t, `{"host_id":"host-a","endpoint":"2001:db8::7","mesh_address":"fdaa:0:0:a1b2::1"}`)
	identity, err := LoadIdentity(path)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	want := HostIdentity{HostID: "host-a", Endpoint: "2001:db8::7", MeshAddress: "fdaa:0:0:a1b2::1"}
	if identity != want {
		t.Fatalf("identity = %+v, want %+v", identity, want)
	}
}

// A missing file is an error — a host the operator forgot to provision must fail
// loud, not come up claiming an empty identity.
func TestLoadIdentityFailsLoudOnMissingFile(t *testing.T) {
	if _, err := LoadIdentity(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected an error for a missing identity file")
	}
}

// A file missing a required field is rejected, distinguishing absent from empty.
func TestLoadIdentityRejectsMissingField(t *testing.T) {
	path := writeIdentity(t, `{"host_id":"host-a","endpoint":"2001:db8::7"}`)
	if _, err := LoadIdentity(path); err == nil {
		t.Fatal("expected an error for an identity file missing mesh_address")
	}
}

func TestLoadIdentityRejectsMalformedJSON(t *testing.T) {
	path := writeIdentity(t, `{not json`)
	if _, err := LoadIdentity(path); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}
