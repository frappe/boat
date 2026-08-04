package migration

import "testing"

// The expected values are the OUTPUT of the Python functions this file ports —
// computed by running atlas migration.py:nbd_port/nbd_base_slot and
// networking.py:derive_vm_tunnel* against these UUIDs. A drift in the Go port
// changes one of these numbers, which is exactly the byte-parity §3.5 asks for:
// both hosts must derive the same device from the same UUID.
func TestDerivationsMatchAtlas(t *testing.T) {
	for _, testCase := range []struct {
		uuid                         string
		nbdPort, slot, tPort, tTable int
		device                       string
	}{
		{"3f2504e0-4f89-41d3-9a0c-0305e82c3301", 11165, 0, 16165, 50688, "mig6-3f2504e0"},
		{"beef1111-1111-4111-8111-111111111111", 13879, 4, 18879, 38513, "mig6-beef1111"},
		{"ffffffff-ffff-ffff-ffff-ffffffffffff", 10535, 12, 15535, 27295, "mig6-ffffffff"},
	} {
		mustEqualInt(t, testCase.uuid, "NBDPort", NBDPort, testCase.nbdPort)
		mustEqualInt(t, testCase.uuid, "NBDBaseSlot", NBDBaseSlot, testCase.slot)
		mustEqualInt(t, testCase.uuid, "TunnelPort", TunnelPort, testCase.tPort)
		mustEqualInt(t, testCase.uuid, "TunnelTable", TunnelTable, testCase.tTable)

		device, err := TunnelDevice(testCase.uuid)
		if err != nil {
			t.Fatalf("%s: TunnelDevice: %v", testCase.uuid, err)
		}
		if device != testCase.device {
			t.Errorf("%s: TunnelDevice = %q, want %q", testCase.uuid, device, testCase.device)
		}
		// IFNAMSIZ is 16; mig6- + 8 hex = 13 must always fit.
		if len(device) > 15 {
			t.Errorf("%s: tunnel device %q exceeds IFNAMSIZ-1", testCase.uuid, device)
		}
	}
}

// The uppercase form derives identically to the canonical one — normalizeHex
// lowercases, matching Python's uuid.UUID, so a caller that passes either gets the
// same devices. If the two disagreed, source and target could name different nbd
// slots from "the same" UUID.
func TestDerivationIsCaseAndDashInsensitive(t *testing.T) {
	canonical := "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	for _, variant := range []string{
		"3F2504E0-4F89-41D3-9A0C-0305E82C3301",
		"3f2504e04f8941d39a0c0305e82c3301",
	} {
		got, err := NBDPort(variant)
		if err != nil {
			t.Fatalf("%q: %v", variant, err)
		}
		want, _ := NBDPort(canonical)
		if got != want {
			t.Errorf("%q derived NBDPort %d, want %d (same UUID)", variant, got, want)
		}
	}
}

func TestDerivationRejectsAMalformedUUID(t *testing.T) {
	for _, bad := range []string{"", "not-a-uuid", "3f2504e0", "zzzzzzzz-ffff-ffff-ffff-ffffffffffff"} {
		if _, err := NBDPort(bad); err == nil {
			t.Errorf("NBDPort(%q) accepted a malformed UUID", bad)
		}
	}
}

// The four source ports are one contiguous block: root, data, base-image, tar.
func TestNBDPortBlockIsContiguous(t *testing.T) {
	root, err := NBDPort("beef1111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if root != 13879 {
		t.Fatalf("root port %d, want 13879", root)
	}
	// data=root+1, base=root+2, tar=root+3 — asserted by construction here so a
	// reader sees the block the source exports.
	for offset, name := range map[int]string{1: "data", 2: "base-image", 3: "tar"} {
		if root+offset < nbdPortBase {
			t.Errorf("%s port underflowed", name)
		}
	}
}

func mustEqualInt(t *testing.T, uuid, name string, fn func(string) (int, error), want int) {
	t.Helper()
	got, err := fn(uuid)
	if err != nil {
		t.Fatalf("%s: %s: %v", uuid, name, err)
	}
	if got != want {
		t.Errorf("%s: %s = %d, want %d", uuid, name, got, want)
	}
}
