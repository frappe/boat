// Package store is Boat's durable memory: the observed state of this host's VMs,
// and the operation journal that makes a retried Atlas Task a replay instead of
// a double-run. One bbolt file, one transaction per call.
//
// Boat is authoritative for its host's observed state, so this file is the only
// thing that survives a daemon restart under VMs that never stopped running.
// Everything here is shaped for the moment an operator has to open it on a
// wedged host: two buckets with obvious names, keyed by the same identifiers
// that appear in Atlas, holding indented JSON.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/frappe/boat/internal/model"
	"go.etcd.io/bbolt"
)

// The two buckets. VMs are keyed by UUID and operations by the Atlas Task name,
// so every key an operator reads out of this file is a key they can search for
// in Atlas.
var (
	virtualMachinesBucket = []byte("virtual-machines")
	operationsBucket      = []byte("operations")
)

// openTimeout bounds the wait for bbolt's exclusive lock on the file. A second
// boat daemon on one host is a misconfiguration, and it should say so within
// seconds rather than hang forever inside flock(2) looking like a slow start.
const openTimeout = 5 * time.Second

// ErrOperationConflict means an identifier was reused for different work.
// Replay is only replay when it is the same operation.
var ErrOperationConflict = errors.New("operation identifier already used for different work")

// errUnclaimedOperation means a completion arrived for an identifier the journal
// has never seen. Every operation is claimed before it runs, so this is a bug in
// the caller and not a state the host can reach on its own.
var errUnclaimedOperation = errors.New("operation was never claimed")

// Store is this host's bbolt database.
type Store struct {
	database *bbolt.DB
}

// Open opens the database at path, creating the parent directory, the file and
// the buckets as needed. It is safe on an existing database: a Boat restart
// re-opens the same file under VMs that are still running.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create store directory for %s: %w", path, err)
	}
	database, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("open store %s: %w", path, err)
	}
	if err := createBuckets(database); err != nil {
		database.Close()
		return nil, err
	}
	return &Store{database: database}, nil
}

func createBuckets(database *bbolt.DB) error {
	return database.Update(func(transaction *bbolt.Tx) error {
		for _, name := range [][]byte{virtualMachinesBucket, operationsBucket} {
			if _, err := transaction.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("create bucket %s: %w", name, err)
			}
		}
		return nil
	})
}

// Close releases the file lock. Reopening the same path afterwards returns the
// same records.
func (store *Store) Close() error {
	return store.database.Close()
}

// PutVirtualMachine records what this host observed about one VM, replacing any
// earlier observation. Observations are latest-wins, not a journal: only the
// operations bucket is append-only.
func (store *Store) PutVirtualMachine(record model.VirtualMachine) error {
	return store.database.Update(func(transaction *bbolt.Tx) error {
		return putRecord(transaction.Bucket(virtualMachinesBucket), record.UUID, record)
	})
}

// GetVirtualMachine reports found=false with a nil error when this host has
// never observed the VM. Absence is an answer, not a failure.
func (store *Store) GetVirtualMachine(uuid string) (model.VirtualMachine, bool, error) {
	var record model.VirtualMachine
	var found bool
	err := store.database.View(func(transaction *bbolt.Tx) error {
		var err error
		record, found, err = getRecord[model.VirtualMachine](transaction.Bucket(virtualMachinesBucket), uuid)
		return err
	})
	if err != nil {
		return model.VirtualMachine{}, false, err
	}
	return record, found, nil
}

// ListVirtualMachines returns every VM this host has observed, ordered by UUID.
// bbolt iterates in key order, so `boat vm ls` is stable between calls for free.
func (store *Store) ListVirtualMachines() ([]model.VirtualMachine, error) {
	records := []model.VirtualMachine{}
	err := store.database.View(func(transaction *bbolt.Tx) error {
		return transaction.Bucket(virtualMachinesBucket).ForEach(func(key, value []byte) error {
			record, err := decodeRecord[model.VirtualMachine](key, value)
			if err != nil {
				return err
			}
			records = append(records, record)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// putRecord stores a record as indented JSON.
//
// bbolt values are opaque bytes, so on a host too wedged to answer its own API
// the only tools an operator has are `strings` and a hex dump of this file.
// Indented JSON survives both readably; a packed encoding does not. The records
// are small and few, so the bytes that costs are cheaper than the legibility it
// buys.
func putRecord(bucket *bbolt.Bucket, key string, record any) error {
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	return bucket.Put([]byte(key), encoded)
}

func getRecord[Record any](bucket *bbolt.Bucket, key string) (Record, bool, error) {
	var record Record
	stored := bucket.Get([]byte(key))
	if stored == nil {
		return record, false, nil
	}
	record, err := decodeRecord[Record]([]byte(key), stored)
	if err != nil {
		return record, false, err
	}
	return record, true, nil
}

// decodeRecord names the key in its error because a record that will not decode
// means a corrupt file, and the first thing anyone will ask is which key it was.
func decodeRecord[Record any](key []byte, value []byte) (Record, error) {
	var record Record
	if err := json.Unmarshal(value, &record); err != nil {
		return record, fmt.Errorf("decode %s: %w", key, err)
	}
	return record, nil
}
