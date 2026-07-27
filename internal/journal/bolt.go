package journal

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

// Every bbolt detail this package has is in this file, and every one of them is
// the same choice internal/store already made — indented JSON, keys an operator
// can grep for, one transaction per call. That is not accidental duplication: it
// is what makes moving the decisions into the store's own file a matter of
// deleting this file, once the store exposes a bucket for them (see the package
// comment).

// The buckets. decisions holds the write-ahead entries themselves, operations is
// the index of which operations have any and under which incarnation, and meta
// holds the incarnation counter.
var (
	decisionsBucket  = []byte("decisions")
	operationsBucket = []byte("operations")
	metaBucket       = []byte("meta")
)

var buckets = [][]byte{decisionsBucket, operationsBucket, metaBucket}

// openTimeout bounds the wait for bbolt's exclusive lock on the file, for the
// reason internal/store gives: a second daemon on one host is a
// misconfiguration, and it should say so in seconds rather than hang inside
// flock(2) looking like a slow start.
const openTimeout = 5 * time.Second

// incarnationKey is the one key in the meta bucket, spelled so that `strings`
// over the file answers "how many times has this daemon started" without a tool.
const incarnationKey = "incarnation"

// openDecisions opens the journal's database, creating the parent directory, the
// file and the buckets as needed.
//
// NoSync is never set, here or anywhere: Record's whole contract is that the
// decision is on the platter when it returns, and bbolt gives that by fsyncing
// at commit. A journal that batched its writes would be a journal whose entries
// are lost by exactly the crash they were written for.
func openDecisions(path string) (*bbolt.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create journal directory for %s: %w", path, err)
	}
	database, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: openTimeout})
	if err != nil {
		return nil, fmt.Errorf("open journal %s: %w", path, err)
	}
	if err := createBuckets(database); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
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

// beginIncarnation advances the run counter and returns the number this run
// holds. It is a read-modify-write with no lock of ours because bbolt admits one
// write transaction at a time, and it is a write rather than a process-lifetime
// random number because the comparison Unfinished makes — "earlier than mine" —
// needs an order, not just an identity.
func beginIncarnation(database *bbolt.DB) (int64, error) {
	var incarnation int64
	err := database.Update(func(transaction *bbolt.Tx) error {
		previous, _, err := getRecord[int64](transaction.Bucket(metaBucket), incarnationKey)
		if err != nil {
			return err
		}
		incarnation = previous + 1
		return putRecord(transaction.Bucket(metaBucket), incarnationKey, incarnation)
	})
	if err != nil {
		return 0, fmt.Errorf("begin a journal incarnation: %w", err)
	}
	return incarnation, nil
}

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

// decodeRecord names the key in its error, because a record that will not decode
// means a corrupt file and the first thing anyone asks is which key it was.
func decodeRecord[Record any](key []byte, value []byte) (Record, error) {
	var record Record
	if err := json.Unmarshal(value, &record); err != nil {
		return record, fmt.Errorf("decode %s: %w", key, err)
	}
	return record, nil
}
