package backup

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/frappe/boat/internal/paths"
	"github.com/frappe/boat/internal/thinpool"
)

// UploadSnapshotParams is the snapshot's name (which names the temp working
// directory) and the object plan the controller signed.
type UploadSnapshotParams struct {
	SnapshotName string
	Objects      []BackupObject
}

// UploadedObject is what one uploaded artifact left behind, so the controller can
// record where it landed and verify it on a later restore.
type UploadedObject struct {
	Name            string
	ObjectName      string
	SHA256          string
	CompressedBytes int64
	RawBytes        int64
}

// UploadSnapshotResult is every uploaded object plus the total compressed size.
type UploadSnapshotResult struct {
	Objects              []UploadedObject
	TotalCompressedBytes int64
}

// UploadSnapshotS3 uploads a snapshot's on-host artifacts to S3 via
// controller-presigned PUT URLs. The host holds NO S3 credentials — it streams
// each object with curl to a short-lived URL. Cold snapshots upload their disk
// LV(s); warm snapshots also upload the frozen memory pair + host signature.
//
// Each object is handled ONE AT A TIME: compress the source to a temp file,
// sha256 it, curl it to the presigned URL, delete the temp. Peak temp space is
// therefore the largest single COMPRESSED object, not the sum. Idempotent: curl
// overwrites, so a re-run re-uploads. Ports scripts/upload-snapshot-s3.py.
func UploadSnapshotS3(ctx context.Context, cmd commands, params UploadSnapshotParams) (UploadSnapshotResult, error) {
	if len(params.Objects) == 0 {
		return UploadSnapshotResult{}, fmt.Errorf("no objects to upload")
	}
	work := paths.AtlasRoot + "/tmp/s3-upload-" + params.SnapshotName
	if _, err := cmd.Run(ctx, "sudo rm -rf {}", work); err != nil {
		return UploadSnapshotResult{}, err
	}
	if err := cmd.InstallDirectory(ctx, work, "0700"); err != nil {
		return UploadSnapshotResult{}, err
	}

	uploaded, uploadErr := uploadAll(ctx, cmd, params.Objects, work)
	// The working directory is always swept, whether the loop succeeded or not —
	// the finally in the Python. A cleanup failure is only reported when the upload
	// itself succeeded, so the real error is never masked by the tidy-up.
	if _, err := cmd.Run(ctx, "sudo rm -rf {}", work); err != nil && uploadErr == nil {
		return UploadSnapshotResult{}, err
	}
	if uploadErr != nil {
		return UploadSnapshotResult{}, uploadErr
	}

	var total int64
	for _, object := range uploaded {
		total += object.CompressedBytes
	}
	return UploadSnapshotResult{Objects: uploaded, TotalCompressedBytes: total}, nil
}

func uploadAll(ctx context.Context, cmd commands, objects []BackupObject, work string) ([]UploadedObject, error) {
	uploaded := make([]UploadedObject, 0, len(objects))
	for _, object := range objects {
		one, err := uploadOne(ctx, cmd, object, work)
		if err != nil {
			return nil, err
		}
		uploaded = append(uploaded, one)
	}
	return uploaded, nil
}

// uploadOne compresses one artifact to a temp file, sha256s it, PUTs it, deletes
// the temp.
func uploadOne(ctx context.Context, cmd commands, object BackupObject, work string) (UploadedObject, error) {
	temp := work + "/" + object.ObjectName
	rawBytes, err := compress(ctx, cmd, object, temp)
	if err != nil {
		return UploadedObject{}, err
	}
	output, err := cmd.Run(ctx, "sudo sha256sum {}", temp)
	if err != nil {
		return UploadedObject{}, err
	}
	digest := strings.Fields(output)
	if len(digest) == 0 {
		return UploadedObject{}, fmt.Errorf("sha256sum gave no digest for %s: %q", temp, output)
	}
	compressedBytes, err := statSize(ctx, cmd, temp)
	if err != nil {
		return UploadedObject{}, err
	}
	if _, err := cmd.Run(ctx, "sudo curl --fail --silent --show-error --upload-file {} {}", temp, object.URL); err != nil {
		return UploadedObject{}, err
	}
	if _, err := cmd.Run(ctx, "sudo rm -f {}", temp); err != nil {
		return UploadedObject{}, err
	}
	return UploadedObject{
		Name:            object.Name,
		ObjectName:      object.ObjectName,
		SHA256:          digest[0],
		CompressedBytes: compressedBytes,
		RawBytes:        rawBytes,
	}, nil
}

// compress writes the compressed (or, for the tiny signature JSON, verbatim)
// artifact to temp and returns the raw source size. zstd reads the LV block device
// (or file) directly — no dd, no pipe — so the exit code is honestly zstd's own.
func compress(ctx context.Context, cmd commands, object BackupObject, temp string) (int64, error) {
	source, rawBytes, err := activatedSource(ctx, cmd, object)
	if err != nil {
		return 0, err
	}
	if object.Compress {
		if _, err := cmd.Run(ctx, "sudo zstd -q -f -{} -T0 -o {} {}", zstdLevel, temp, source); err != nil {
			return 0, err
		}
	} else if _, err := cmd.Run(ctx, "sudo cp {} {}", source, temp); err != nil {
		return 0, err
	}
	return rawBytes, nil
}

// activatedSource resolves the readable source path and its raw byte size: a plain
// file as-is, or an activated LV device. A missing source fails loud — a backup
// cannot upload bytes that are not there.
func activatedSource(ctx context.Context, cmd commands, object BackupObject) (string, int64, error) {
	if !object.Block {
		if !cmd.OK(ctx, "sudo test -f {}", object.Source) {
			return "", 0, fmt.Errorf("source file missing: %s", object.Source)
		}
		size, err := statSize(ctx, cmd, object.Source)
		return object.Source, size, err
	}
	name := thinpool.NameFromDevice(object.Source)
	if !thinpool.Exists(ctx, cmd, name) {
		return "", 0, fmt.Errorf("source LV not found: %s (%s)", object.Source, name)
	}
	if err := thinpool.Activate(ctx, cmd, name); err != nil {
		return "", 0, err
	}
	device := thinpool.DevicePath(name)
	size, err := thinpool.DeviceSizeBytes(ctx, cmd, device)
	return device, size, err
}

// statSize reads a plain file's byte size via stat.
func statSize(ctx context.Context, cmd commands, path string) (int64, error) {
	output, err := cmd.Run(ctx, "sudo stat -c %s {}", path)
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("stat gave no size for %s: %q", path, output)
	}
	return size, nil
}
