package backup

import "testing"

// ParseObjects decodes the controller's JSON plan and rejects an empty one — a
// backup with no artifacts is a bug, not a silent no-op.
func TestParseObjects(t *testing.T) {
	plan := `[
		{"name":"rootfs","object_name":"rootfs.zst","source":"/dev/atlas/atlas-snap-x",
		 "block":true,"compress":true,"disk_gigabytes":28,"url":"https://put","sha256":"abc"},
		{"name":"mem","object_name":"mem.zst","source":"/var/lib/atlas/snapshots/x/mem.bin",
		 "block":false,"compress":true}
	]`
	objects, err := ParseObjects(plan)
	if err != nil {
		t.Fatalf("ParseObjects: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("parsed %d objects, want 2", len(objects))
	}
	if !objects[0].Block || objects[0].DiskGigabytes != 28 || objects[0].SHA256 != "abc" {
		t.Errorf("object 0 = %+v", objects[0])
	}
	if objects[1].Block || objects[1].DiskGigabytes != 0 {
		t.Errorf("object 1 = %+v", objects[1])
	}
}

func TestParseObjectsRejectsEmpty(t *testing.T) {
	for _, plan := range []string{"[]", "  [] "} {
		if _, err := ParseObjects(plan); err == nil {
			t.Errorf("ParseObjects(%q) accepted an empty plan", plan)
		}
	}
}

func TestParseObjectsRejectsGarbage(t *testing.T) {
	if _, err := ParseObjects("not json"); err == nil {
		t.Error("ParseObjects accepted non-JSON")
	}
}
