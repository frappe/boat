package main

import (
	"context"
	"io"

	"github.com/frappe/boat/internal/backup"
	"github.com/frappe/boat/internal/run"
)

// uploadSnapshotS3 ships a snapshot's volumes to object storage — `boat
// upload-snapshot-s3`, the port of upload-snapshot-s3.py.
//
// The objects arrive as one JSON argument rather than as flags, because each
// carries a presigned URL: the host never holds a credential, and the
// controller's signature is the whole authorisation.
func uploadSnapshotS3(arguments []string, output io.Writer, errorOutput io.Writer) int {
	flags := newTaskFlags("upload-snapshot-s3", errorOutput)
	name := flags.requiredText("snapshot-name")
	objectsJSON := flags.requiredText("objects-json")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	objects, err := backup.ParseObjects(*objectsJSON)
	if err != nil {
		return reportError(errorOutput, err)
	}
	result, err := backup.UploadSnapshotS3(context.Background(), run.NewRunner(errorOutput), backup.UploadSnapshotParams{
		SnapshotName: *name,
		Objects:      objects,
	})
	if err != nil {
		return reportError(errorOutput, err)
	}
	return emit(output, errorOutput, map[string]any{
		"objects":                uploadedObjects(result.Objects),
		"total_compressed_bytes": result.TotalCompressedBytes,
	})
}

// restoreSnapshotS3 rehydrates a snapshot's volumes from object storage —
// `boat restore-snapshot-s3`, the port of restore-snapshot-s3.py.
func restoreSnapshotS3(arguments []string, output io.Writer, errorOutput io.Writer) int {
	flags := newTaskFlags("restore-snapshot-s3", errorOutput)
	name := flags.requiredText("snapshot-name")
	objectsJSON := flags.requiredText("objects-json")
	if err := flags.parse(arguments); err != nil {
		return reportError(errorOutput, err)
	}
	objects, err := backup.ParseObjects(*objectsJSON)
	if err != nil {
		return reportError(errorOutput, err)
	}
	result, err := backup.RestoreSnapshotS3(context.Background(), run.NewRunner(errorOutput), backup.RestoreSnapshotParams{
		SnapshotName: *name,
		Objects:      objects,
	})
	if err != nil {
		return reportError(errorOutput, err)
	}
	// An empty restore is `[]`, never `null`: the controller's dataclass field
	// is a list, and JSON null would reach it as None.
	names := result.Objects
	if names == nil {
		names = []string{}
	}
	return emit(output, errorOutput, map[string]any{"objects": names})
}

// uploadedObjects renders each uploaded object as the dict the controller's
// result field is documented to hold: {name, object_name, sha256,
// compressed_bytes, raw_bytes}.
func uploadedObjects(objects []backup.UploadedObject) []map[string]any {
	rendered := make([]map[string]any, 0, len(objects))
	for _, object := range objects {
		rendered = append(rendered, map[string]any{
			"name":             object.Name,
			"object_name":      object.ObjectName,
			"sha256":           object.SHA256,
			"compressed_bytes": object.CompressedBytes,
			"raw_bytes":        object.RawBytes,
		})
	}
	return rendered
}
