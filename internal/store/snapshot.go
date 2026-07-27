package store

import (
	"time"

	"github.com/frappe/boat/internal/model"
	"go.etcd.io/bbolt"
)

// Snapshot materializes everything an export needs inside one short read
// transaction and returns it as a value, before a byte of it is written out.
//
// The transaction is closed by the time this returns, and that is the whole
// point. Serialising an export to a client is unbounded work: the client may be
// slow, far away, or stopped. Doing it with a store transaction still open would
// put the file's page reclamation behind whoever the slowest reader on this host
// happens to be, and a host that grows slow at answering is a host Atlas
// declares partitioned — which is a host Atlas stops scheduling onto. A slow
// reader must never be able to make a healthy host look dead.
//
// ObservedEpoch is read in the same transaction as the records it describes, so
// the two cannot disagree: bbolt gives a read transaction one consistent view of
// the file, and every write that changes anything read here bumps that epoch in
// the same transaction as the change. A caller may therefore CAS against the
// epoch it received knowing it belongs to exactly the state it acted on.
//
// Host, Units and LogicalVolumes are left zero for the caller to enrich. The
// store cannot observe them, and filling in empty slices would report "this host
// has no logical volumes" — a claim it is not entitled to make.
func (store *Store) Snapshot() (model.Export, error) {
	var export model.Export
	err := store.database.View(func(transaction *bbolt.Tx) error {
		var err error
		export, err = snapshot(transaction)
		return err
	})
	if err != nil {
		return model.Export{}, err
	}
	return export, nil
}

func snapshot(transaction *bbolt.Tx) (model.Export, error) {
	export := model.Export{TakenAt: time.Now().UTC()}
	var err error
	if export.ObservedEpoch, err = observedEpoch(transaction); err != nil {
		return model.Export{}, err
	}
	if export.VirtualMachines, err = listVirtualMachines(transaction); err != nil {
		return model.Export{}, err
	}
	if export.FenceEpochs, err = fenceEpochs(transaction); err != nil {
		return model.Export{}, err
	}
	if export.Quarantined, err = quarantined(transaction); err != nil {
		return model.Export{}, err
	}
	return export, nil
}
