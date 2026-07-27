package store

import (
	"fmt"

	"github.com/frappe/boat/internal/model"
	"go.etcd.io/bbolt"
)

// ReplaceQuarantine records everything this host holds that could not be read
// as a virtual machine, replacing whatever the last scan recorded.
//
// Latest-wins rather than append-only, and deliberately so: quarantine
// describes the host as it is now. An operator who repairs a half-terminated VM
// must see it stop being reported, and a journal of every artifact set that was
// ever ambiguous would bury the ones that still are.
//
// It bumps the observed epoch, because quarantine is part of the export and a
// snapshot whose epoch did not move would let a caller act on a host whose
// reported state had changed underneath them.
func (store *Store) ReplaceQuarantine(records []model.Quarantine) error {
	return store.database.Update(func(transaction *bbolt.Tx) error {
		if err := transaction.DeleteBucket(quarantineBucket); err != nil && err != bbolt.ErrBucketNotFound {
			return fmt.Errorf("clear quarantine: %w", err)
		}
		bucket, err := transaction.CreateBucket(quarantineBucket)
		if err != nil {
			return fmt.Errorf("create quarantine: %w", err)
		}
		for _, record := range records {
			if err := putRecord(bucket, record.UUID, record); err != nil {
				return err
			}
		}
		_, err = bumpObservedEpoch(transaction)
		return err
	})
}

// quarantined reads the quarantine set inside the caller's transaction, so an
// export cannot report VMs from one instant and quarantine from another.
func quarantined(transaction *bbolt.Tx) ([]model.Quarantine, error) {
	records := []model.Quarantine{}
	bucket := transaction.Bucket(quarantineBucket)
	if bucket == nil {
		return records, nil
	}
	err := bucket.ForEach(func(key, value []byte) error {
		record, err := decodeRecord[model.Quarantine](key, value)
		if err != nil {
			return err
		}
		records = append(records, record)
		return nil
	})
	return records, err
}
