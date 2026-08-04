// Package backup moves a snapshot's on-host artifacts to and from S3 — the disk
// LV(s), and for a warm snapshot the frozen memory pair — WITHOUT the host ever
// holding an S3 credential. Atlas signs a short-lived URL per object and the host
// streams the bytes straight to or from it with curl. "Atlas never proxies bytes":
// the controller signs, the host transfers.
//
// UploadSnapshotS3 compresses each artifact to a temp file (an S3 PUT needs a
// known length, so an unknown-length stream cannot be piped), sha256s it and PUTs
// it. RestoreSnapshotS3 pulls each object, VERIFIES its sha256 BEFORE decompress —
// the integrity gate — then decompresses straight onto a freshly recreated thin
// LV (or into a memory file). Both handle one object at a time, so peak temp space
// is the largest single COMPRESSED object, not the sum. Idempotent: an upload
// overwrites, a restore recreates the LV clean.
//
// It ports scripts/{upload-snapshot-s3,restore-snapshot-s3}.py and
// scripts/lib/atlas/snapshot_backup.py (the object plan). The LVM mechanics live
// in internal/thinpool. Every command goes through the run seam, so the whole
// package is unit-testable with no curl, no zstd, no LVM stack and no root. See
// spec/29-snapshot-backup.md.
package backup

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/frappe/boat/internal/run"
)

// zstdLevel favours a fast, well-parallelised compress over the last few percent
// of ratio: level 3 (zstd's default) with every core.
const zstdLevel = 3

// commands is everything this package does to the host, and the only seam it has.
// A superset of internal/thinpool's seam, so a verb hands the same value straight
// to the shared LVM helpers. Outside tests the one implementation is *run.Runner.
type commands interface {
	Run(ctx context.Context, template string, parameters ...any) (string, error)
	RunUnchecked(ctx context.Context, template string, parameters ...any) (string, error)
	OK(ctx context.Context, template string, parameters ...any) bool
	Input(ctx context.Context, stdin string, template string, parameters ...any) (string, error)
	InstallDirectory(ctx context.Context, destination string, mode string) error
}

var _ commands = (*run.Runner)(nil)

// BackupObject is one artifact to move to or from S3.
//
// Source is the on-host path — an LV device (Block=true) or a plain file (the warm
// memory pair). On restore it is the DESTINATION (the same path the snapshot row
// records), so upload and restore share this shape. DiskGigabytes is the LV size
// to recreate on restore (0 for a file). Compress is zstd for everything but the
// tiny host-signature JSON. URL is the presigned PUT (upload) or GET (restore);
// SHA256 is the expected digest of the compressed bytes (restore only). The JSON
// tags are the controller's wire contract. Ports snapshot_backup.py BackupObject.
type BackupObject struct {
	Name          string `json:"name"`
	ObjectName    string `json:"object_name"`
	Source        string `json:"source"`
	Block         bool   `json:"block"`
	Compress      bool   `json:"compress"`
	DiskGigabytes int    `json:"disk_gigabytes"`
	URL           string `json:"url"`
	SHA256        string `json:"sha256"`
}

// ParseObjects parses the controller's `--objects-json` plan into typed objects,
// for the API layer that receives it as a JSON string. It raises on empty — a
// backup with no artifacts is a bug, not a silent no-op. Ports
// snapshot_backup.py parse_objects.
func ParseObjects(objectsJSON string) ([]BackupObject, error) {
	var objects []BackupObject
	if err := json.Unmarshal([]byte(objectsJSON), &objects); err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		return nil, errors.New("objects-json is empty: nothing to move")
	}
	return objects, nil
}
