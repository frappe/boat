// Load this host's own identity, ported from identity.py (spec §8 bootstrap
// contract).
//
// The identity file is the part of the bootstrap contract SPECIFIC to this host —
// the seed file, by contrast, lists every OTHER known host. It carries:
//
//	{
//	  "host_id": "<uuid>",                 // the Frappe Server UUID at provision
//	  "endpoint": "2001:db9::7",           // this host's public IPv6 (no port)
//	  "mesh_address": "fdaa:0:0:a1b2::1"   // derived HKDF infra /128 (spec §7.1)
//	}
//
// mesh_address is derived from host_id controller-side and pre-computed here at
// provision so the host needs none of the controller's HKDF code. The keypair is
// separate (keys.go).
//
// This is a pure reader: a small typed HostIdentity and a Load that fails loud on a
// missing or malformed file. A fresh host whose operator forgot to provision the
// identity file must NOT come up peer-empty and silently broadcast gen-1 records
// claiming an empty identity — that would pollute the cluster.
package networkd

import (
	"encoding/json"
	"fmt"
	"os"
)

// DefaultIdentityPath is where bootstrap writes the identity file.
const DefaultIdentityPath = "/etc/atlas-networkd/identity.json"

// HostIdentity is this host's own identity — the fields the daemon needs to
// self-identify in Membership Records and to know which public IPv6 to advertise as
// its wg endpoint. Mirrors the bootstrap contract (spec §8).
type HostIdentity struct {
	HostID      HostID
	Endpoint    string // bare public IPv6
	MeshAddress IP6    // fdaa:0:0:<idx>::1 — the infra /48 bus /128
}

// LoadIdentity reads the identity file. A missing file is an error (provisioning
// did not write it — fail loud, do not invent an empty identity); a malformed file,
// or one missing any of the three required fields, is an error too.
func LoadIdentity(path string) (HostIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return HostIdentity{}, fmt.Errorf("identity file at %s: %w", path, err)
	}
	// A pointer-per-field decode distinguishes "absent" from "present but empty":
	// json leaves an absent field nil, so a file missing mesh_address is rejected
	// rather than read as an empty mesh address the host would then advertise.
	var raw struct {
		HostID      *string `json:"host_id"`
		Endpoint    *string `json:"endpoint"`
		MeshAddress *string `json:"mesh_address"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return HostIdentity{}, fmt.Errorf("identity file at %s is not valid JSON: %w", path, err)
	}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"host_id", raw.HostID},
		{"endpoint", raw.Endpoint},
		{"mesh_address", raw.MeshAddress},
	} {
		if field.value == nil {
			return HostIdentity{}, fmt.Errorf("identity file at %s missing field %q", path, field.name)
		}
	}
	return HostIdentity{HostID: *raw.HostID, Endpoint: *raw.Endpoint, MeshAddress: *raw.MeshAddress}, nil
}
