// Package localownership reads and atomically updates the local-ownership cache
// the ANCP network daemon scans — `/etc/atlas-networkd/local-ownership.json`, a
// flat list of the /128s this host locally owns.
//
// The seam is deliberate and is ANCP's, not Boat's (spec/31 §11.3, spec/33 §6.1):
// atlas-networkd never consults a database or listens for VM lifecycle events. It
// reads this file on a timer and advertises whatever set it finds. The VM
// lifecycle is the writer — a bring-up adds the VM's /128, a teardown removes it —
// and Boat becomes that writer without ANCP reading one line of it differently.
// Boat supervises the daemon's lifecycle and touches none of its gossip state; it
// writes an address list, nothing more.
//
// The writers serialize under a POSIX flock because two concurrent bring-ups
// would otherwise each read the same baseline, each write back only its own
// addition, and the second rename would silently drop the first VM's /128. The
// daemon only ever reads, and reads the finished file after the writer releases,
// so the lock serializes writers alone.
//
// Ported from scripts/lib/atlas/networkd/localownership.py, flock, atomic rename
// and fail-loud-on-corrupt included. This is the writer the WO-3b network-up and
// network-down apply will call; it is landed ahead of them the way WO-2's
// components were built before they were wired.
package localownership

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

const (
	// DefaultPath is the cache atlas-networkd scans, in the daemon's own directory
	// that bootstrap creates and a host reset keeps.
	DefaultPath = "/etc/atlas-networkd/local-ownership.json"

	// lockBaseName is the writers' shared flock, co-located with the cache. For
	// DefaultPath that resolves to /etc/atlas-networkd/local-ownership.lock — the
	// exact file scripts/lib/atlas/networkd/localownership.py locks — so a Python
	// bring-up and a Go one on the same host still take turns, and a test using a
	// temp cache locks a temp file rather than needing root to touch /etc.
	lockBaseName = "local-ownership.lock"

	ownedKey = "owned"
)

// Read returns the /128s the host locally owns, sorted, per the cache at path.
//
// A missing file is an empty set, not an error: the daemon is fresh and no VM has
// come up yet. A present but malformed file is an error, loud on purpose — a
// silent empty set on a corrupt cache would advertise "I own nothing" and could
// withdraw routes the host is still carrying. An empty `{}` or `{"owned": []}` is
// legitimate and returns no addresses.
func Read(path string) ([]string, error) {
	_, owned, err := load(path)
	return owned, err
}

// Add atomically adds address to the cache at path, creating the file on the
// first add. A read-modify-write under the exclusive flock, and a no-op when the
// address is already present. Extra top-level fields the daemon may later add are
// preserved across the write.
func Add(path string, address string) error {
	return mutate(path, func(owned []string) ([]string, bool) {
		if slices.Contains(owned, address) {
			return nil, false
		}
		return insertSorted(owned, address), true
	})
}

// Remove atomically removes address from the cache. The teardown twin of Add: a
// no-op when the cache is missing or the address is not in it, so a detach that
// runs after the VM is already gone is not an error. The daemon's next scan sees
// the smaller set and advertises the withdrawal.
func Remove(path string, address string) error {
	return mutate(path, func(owned []string) ([]string, bool) {
		index := slices.Index(owned, address)
		if index < 0 {
			return nil, false
		}
		return slices.Delete(slices.Clone(owned), index, index+1), true
	})
}

// mutate serializes a read-modify-write under the flock, writes only when the
// change function reports a change, and preserves every top-level field but
// `owned` — so a schema-version stamp a future daemon writes survives a Boat that
// predates it, exactly as the Python's `{**read, "owned": ...}` merge does.
func mutate(path string, change func(owned []string) ([]string, bool)) (err error) {
	release, err := lock(path)
	if err != nil {
		return err
	}
	defer release()

	document, owned, err := load(path)
	if err != nil {
		return err
	}
	next, changed := change(owned)
	if !changed {
		return nil
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	document[ownedKey] = encoded
	return writeAtomic(path, document)
}

// load reads the cache into its raw top-level document and the sorted owned set.
// A missing file is an empty document and no addresses. A malformed file, or one
// whose `owned` is not a list of strings, is an error — the same fail-loud read
// Read exposes, so a corrupt cache never round-trips through a writer as truth.
func load(path string) (map[string]json.RawMessage, []string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, nil, fmt.Errorf("local-ownership cache at %s is not a JSON object: %w", path, err)
	}
	raw, present := document[ownedKey]
	if !present {
		return nil, nil, fmt.Errorf("local-ownership cache at %s has no %q", path, ownedKey)
	}
	var owned []string
	if err := json.Unmarshal(raw, &owned); err != nil {
		return nil, nil, fmt.Errorf("local-ownership cache at %s: %q is not a list of addresses: %w", path, ownedKey, err)
	}
	slices.Sort(owned)
	return document, owned, nil
}

// insertSorted returns owned with address added, kept sorted, without mutating
// the input.
func insertSorted(owned []string, address string) []string {
	next := append(slices.Clone(owned), address)
	slices.Sort(next)
	return next
}

// lock acquires the exclusive advisory flock the writers share, co-located with
// the cache and created with its directory on first use. The returned release
// closes the fd, which drops the lock. It blocks until the lock is held, so two
// concurrent bring-ups take their turns rather than clobbering each other.
func lock(cachePath string) (func(), error) {
	path := filepath.Join(filepath.Dir(cachePath), lockBaseName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, nil
}

// writeAtomic writes document to path via a temp file and a rename, so a daemon
// mid-scan sees either the whole old file or the whole new one, never a truncated
// one. The parent directory is fsync'd after the rename so the replace survives a
// power cut — a just-added VM's /128 must not vanish on the next boot. Map keys
// marshal in sorted order, so the file is deterministic.
func writeAtomic(path string, document map[string]json.RawMessage) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if err := writeAndClose(temporary, append(encoded, '\n')); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return fsyncDirectory(directory)
}

// writeAndClose writes body to file, flushes it to disk, and closes it. The fsync
// is what makes the following rename durable rather than merely visible.
func writeAndClose(file *os.File, body []byte) error {
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// fsyncDirectory flushes a directory entry so a preceding rename into it survives
// a crash. Best-effort on the open; a directory that cannot be opened for sync is
// not worth failing a completed write over.
func fsyncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return nil
	}
	defer handle.Close()
	return handle.Sync()
}
