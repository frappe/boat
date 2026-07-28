// Package store is Boat's durable memory: the observed state of this host's VMs,
// the desired state Atlas has asserted, the fence epochs that decide what may
// boot, and the operation journal — the claims that make a retried Atlas Task a
// replay instead of a double-run, and the write-ahead decisions taken under
// them. One bbolt file, one transaction per call, and one file on purpose: a
// decision that did not commit with the record authorizing it is a decision a
// crash can strand (spec/33-boat.md §11.5).
//
// Boat is authoritative for its host's observed state, so this file is the only
// thing that survives a daemon restart under VMs that never stopped running.
// Everything here is shaped for the moment an operator has to open it on a
// wedged host: a handful of buckets with obvious names, keyed by the same
// identifiers that appear in Atlas, holding indented JSON.
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

// The buckets. VMs, desired records and fence epochs are keyed by UUID,
// operations by the Atlas Task name and decisions by that name plus a sequence,
// so every key an operator reads out of this file is a key they can search for
// in Atlas. meta holds the store's own bookkeeping: the observed epoch and the
// incarnation counter.
var (
	virtualMachinesBucket = []byte("virtual-machines")
	// quarantineBucket holds artifact sets that could not be read as a VM. It is
	// latest-wins per scan rather than a journal: quarantine describes the host
	// as it is now, and a resolved one must stop being reported.
	quarantineBucket = []byte("quarantine")
	operationsBucket = []byte("operations")
	// decisionsBucket holds the write-ahead decisions of spec/33-boat.md §11.5,
	// keyed by the operation that took them. It is in this file rather than beside
	// it because the rule asks for a decision and the state it justifies to commit
	// in ONE transaction, and bbolt takes an exclusive lock per file — so a second
	// file would be a second commit, and a crash between the two is exactly the
	// gap the rule exists to close. See decisions.go.
	decisionsBucket = []byte("decisions")
	desiredBucket   = []byte("desired")
	fenceBucket     = []byte("fence")
	metaBucket      = []byte("meta")
)

// buckets is the whole set, so that adding one is a single edit rather than an
// edit plus a matching line in Open that is easy to forget.
var buckets = [][]byte{
	virtualMachinesBucket,
	quarantineBucket,
	operationsBucket,
	decisionsBucket,
	desiredBucket,
	fenceBucket,
	metaBucket,
}

// openTimeout bounds the wait for bbolt's exclusive lock on the file. A second
// boat daemon on one host is a misconfiguration, and it should say so within
// seconds rather than hang forever inside flock(2) looking like a slow start.
const openTimeout = 5 * time.Second

// ErrOperationConflict means an identifier was reused for different work.
// Replay is only replay when it is the same operation.
var ErrOperationConflict = errors.New("operation identifier already used for different work")

// errUnclaimedOperation means a completion or a decision arrived for an
// identifier the journal has never seen. Every operation is claimed before it
// runs, so this is a bug in the caller and not a state the host can reach on its
// own.
var errUnclaimedOperation = errors.New("operation was never claimed")

// Store is this host's bbolt database.
type Store struct {
	database *bbolt.DB
	// incarnation is which run of this daemon holds the file. See incarnation.go.
	incarnation int64
}

// Open opens the database at path, creating the parent directory, the file and
// the buckets as needed. It is safe on an existing database: a Boat restart
// re-opens the same file under VMs that are still running.
//
// Opening is itself a durable act, because it advances the incarnation counter:
// every operation claimed from here on is stamped with a number no earlier run
// can have used, which is what makes a crashed operation distinguishable from a
// slow one.
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
	incarnation, err := beginIncarnation(database)
	if err != nil {
		database.Close()
		return nil, err
	}
	return &Store{database: database, incarnation: incarnation}, nil
}

func createBuckets(database *bbolt.DB) error {
	return database.Update(func(transaction *bbolt.Tx) error {
		for _, name := range buckets {
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
//
// The record and the observed-epoch bump land in one transaction. Callers CAS
// against the epoch they read out of a Snapshot, so an epoch that could lag the
// write it describes would hand a caller a token for state it never read, and
// the caller would act on it believing nothing had changed underneath.
func (store *Store) PutVirtualMachine(record model.VirtualMachine) error {
	return store.database.Update(func(transaction *bbolt.Tx) error {
		if err := putRecord(transaction.Bucket(virtualMachinesBucket), record.UUID, record); err != nil {
			return err
		}
		_, err := bumpObservedEpoch(transaction)
		return err
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
	var records []model.VirtualMachine
	err := store.database.View(func(transaction *bbolt.Tx) error {
		var err error
		records, err = listVirtualMachines(transaction)
		return err
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// listVirtualMachines takes the caller's transaction because Snapshot has to
// read the VMs, the fence epochs and the epoch that describes them at one
// instant of the file, which it cannot do by calling three exported methods.
func listVirtualMachines(transaction *bbolt.Tx) ([]model.VirtualMachine, error) {
	records := []model.VirtualMachine{}
	err := transaction.Bucket(virtualMachinesBucket).ForEach(func(key, value []byte) error {
		record, err := decodeRecord[model.VirtualMachine](key, value)
		if err != nil {
			return err
		}
		records = append(records, record)
		return nil
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
