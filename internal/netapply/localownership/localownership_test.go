package localownership

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func cachePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "local-ownership.json")
}

// A cache no VM has written yet is an empty set, not a failure: the daemon is
// fresh, and this is the state every host boots into.
func TestReadOfAMissingCacheIsEmptyNotAnError(t *testing.T) {
	owned, err := Read(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Read of a missing cache: %v", err)
	}
	if len(owned) != 0 {
		t.Errorf("Read of a missing cache = %v, want empty", owned)
	}
}

// A corrupt cache raises rather than reading as empty: an empty set would
// advertise "I own nothing" and could withdraw routes the host still carries.
func TestReadOfACorruptCacheRaises(t *testing.T) {
	for _, testCase := range []struct{ name, body string }{
		{"not JSON at all", "{not json"},
		{"a JSON list, not an object", `["fdaa:0:1::5"]`},
		{"an object with no owned key", `{"schema_version": 1}`},
		{"owned is not a list", `{"owned": "fdaa:0:1::5"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := cachePath(t)
			if err := os.WriteFile(path, []byte(testCase.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(path); err == nil {
				t.Errorf("Read of %q did not raise", testCase.body)
			}
		})
	}
}

// A cache the Python writer produced reads here, addresses sorted — the interop
// that keeps ANCP converging with Boat as the cache's writer.
func TestReadOfAPythonWrittenCacheReturnsTheSortedSet(t *testing.T) {
	path := cachePath(t)
	body := `{"owned": ["fdaa:0:1::5", "fdaa:0:1::2"], "schema_version": 1}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	owned, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []string{"fdaa:0:1::2", "fdaa:0:1::5"}
	if fmt.Sprint(owned) != fmt.Sprint(want) {
		t.Errorf("Read = %v, want %v", owned, want)
	}
}

// The first add creates the cache; a second add of the same address changes
// nothing; a third adds and keeps the set sorted.
func TestAddCreatesAndIsIdempotentAndSorts(t *testing.T) {
	path := cachePath(t)
	if err := Add(path, "fdaa:0:1::5"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := Add(path, "fdaa:0:1::5"); err != nil {
		t.Fatalf("idempotent add: %v", err)
	}
	if err := Add(path, "fdaa:0:1::2"); err != nil {
		t.Fatalf("second address: %v", err)
	}
	owned, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if want := []string{"fdaa:0:1::2", "fdaa:0:1::5"}; fmt.Sprint(owned) != fmt.Sprint(want) {
		t.Errorf("owned = %v, want %v", owned, want)
	}
}

// A write preserves every top-level field but owned, so a schema stamp the daemon
// adds outlives a Boat that has never heard of it.
func TestAddPreservesUnknownTopLevelFields(t *testing.T) {
	path := cachePath(t)
	body := `{"owned": ["fdaa:0:1::5"], "schema_version": 7}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Add(path, "fdaa:0:1::2"); err != nil {
		t.Fatalf("add: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"schema_version":7`; !contains(string(written), want) {
		t.Errorf("the unknown field was dropped: %s", written)
	}
}

// Remove is the teardown twin: it drops a present address, is a no-op on one that
// is absent, and a no-op on a cache that does not exist at all.
func TestRemoveDropsPresentAndToleratesAbsent(t *testing.T) {
	path := cachePath(t)
	if err := Add(path, "fdaa:0:1::5"); err != nil {
		t.Fatal(err)
	}
	if err := Add(path, "fdaa:0:1::2"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(path, "fdaa:0:1::5"); err != nil {
		t.Fatalf("remove present: %v", err)
	}
	if err := Remove(path, "fdaa:0:1::5"); err != nil {
		t.Fatalf("remove absent: %v", err)
	}
	owned, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if want := []string{"fdaa:0:1::2"}; fmt.Sprint(owned) != fmt.Sprint(want) {
		t.Errorf("owned = %v, want %v", owned, want)
	}
	if err := Remove(filepath.Join(t.TempDir(), "missing.json"), "fdaa:0:1::9"); err != nil {
		t.Errorf("remove from a missing cache: %v", err)
	}
}

// The whole reason for the flock: concurrent adds must not clobber each other.
// Twenty goroutines each add a distinct address, and all twenty must survive —
// without the lock, the last rename wins and most are lost. Run under -race.
func TestConcurrentAddsDoNotClobberEachOther(t *testing.T) {
	path := cachePath(t)
	const count = 20
	var group sync.WaitGroup
	for index := 0; index < count; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			if err := Add(path, fmt.Sprintf("fdaa:0:1::%d", index)); err != nil {
				t.Errorf("add %d: %v", index, err)
			}
		}(index)
	}
	group.Wait()

	owned, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(owned) != count {
		t.Errorf("after %d concurrent adds the cache holds %d: %v", count, len(owned), owned)
	}
}

func contains(haystack, needle string) bool {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
