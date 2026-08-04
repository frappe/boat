package backup

import (
	"context"
	"testing"
)

const (
	uploadWork = "/var/lib/atlas/tmp/s3-upload-snap-golden"
	snapLV     = "atlas-snap-golden"
	snapDevice = "/dev/atlas/atlas-snap-golden"
	signature  = "/var/lib/atlas/snapshots/snap-golden/host-signature.json"
)

// A warm snapshot's upload: a block disk LV (activated, its raw size read, then
// zstd-compressed) and the tiny signature file (copied verbatim, not compressed).
// Each object is compressed, sha256'd, PUT to its presigned URL and deleted; the
// working directory is swept at the end.
func TestUploadSnapshotS3BlockAndFile(t *testing.T) {
	rootfsTemp := uploadWork + "/rootfs.zst"
	signatureTemp := uploadWork + "/host-signature.json"
	fake := newFakeCommands().
		exists("sudo lvs --noheadings atlas/"+snapLV).
		exists("test -b "+snapDevice).
		output("sudo blockdev --getsize64 "+snapDevice, "10737418240\n").
		output("sudo sha256sum "+rootfsTemp, "aaa111  "+rootfsTemp+"\n").
		output("sudo stat -c %s "+rootfsTemp, "5000000").
		exists("sudo test -f "+signature).
		output("sudo stat -c %s "+signature, "200").
		output("sudo sha256sum "+signatureTemp, "bbb222  "+signatureTemp+"\n").
		output("sudo stat -c %s "+signatureTemp, "200")

	objects := []BackupObject{
		{Name: "rootfs", ObjectName: "rootfs.zst", Source: snapDevice, Block: true, Compress: true, URL: "https://put-rootfs"},
		{Name: "host-signature", ObjectName: "host-signature.json", Source: signature, Block: false, Compress: false, URL: "https://put-sig"},
	}
	result, err := UploadSnapshotS3(context.Background(), fake, UploadSnapshotParams{
		SnapshotName: "snap-golden", Objects: objects,
	})
	if err != nil {
		t.Fatalf("UploadSnapshotS3: %v", err)
	}
	if result.TotalCompressedBytes != 5000200 || len(result.Objects) != 2 {
		t.Errorf("result = %+v", result)
	}
	if result.Objects[0].SHA256 != "aaa111" || result.Objects[0].RawBytes != 10737418240 || result.Objects[0].CompressedBytes != 5000000 {
		t.Errorf("rootfs object = %+v", result.Objects[0])
	}
	if result.Objects[1].SHA256 != "bbb222" || result.Objects[1].RawBytes != 200 {
		t.Errorf("signature object = %+v", result.Objects[1])
	}

	assertTrace(t, fake,
		"sudo rm -rf "+uploadWork,
		"install-dir 0700 "+uploadWork,
		// block object: activate, read raw size, compress, hash, size, PUT, delete
		"? sudo lvs --noheadings atlas/"+snapLV,
		"sudo lvchange -ay -K atlas/"+snapLV,
		"sudo udevadm settle",
		"? test -b "+snapDevice,
		"sudo blockdev --getsize64 "+snapDevice,
		"sudo zstd -q -f -3 -T0 -o "+rootfsTemp+" "+snapDevice,
		"sudo sha256sum "+rootfsTemp,
		"sudo stat -c %s "+rootfsTemp,
		"sudo curl --fail --silent --show-error --upload-file "+rootfsTemp+" https://put-rootfs",
		"sudo rm -f "+rootfsTemp,
		// file object: verify present, read raw size, copy verbatim, hash, size, PUT, delete
		"? sudo test -f "+signature,
		"sudo stat -c %s "+signature,
		"sudo cp "+signature+" "+signatureTemp,
		"sudo sha256sum "+signatureTemp,
		"sudo stat -c %s "+signatureTemp,
		"sudo curl --fail --silent --show-error --upload-file "+signatureTemp+" https://put-sig",
		"sudo rm -f "+signatureTemp,
		// cleanup
		"sudo rm -rf "+uploadWork,
	)
}

// The host holds no S3 credentials — every transfer is a curl to a presigned URL,
// never an aws/s3 client, never a bucket name.
func TestUploadSnapshotS3HoldsNoCredentials(t *testing.T) {
	fake := newFakeCommands().
		exists("sudo test -f "+signature).
		output("sudo stat -c %s "+signature, "200").
		output("sudo sha256sum "+uploadWork+"/sig", "ccc333  x").
		output("sudo stat -c %s "+uploadWork+"/sig", "200")
	_, err := UploadSnapshotS3(context.Background(), fake, UploadSnapshotParams{
		SnapshotName: "snap-golden",
		Objects:      []BackupObject{{Name: "sig", ObjectName: "sig", Source: signature, URL: "https://put"}},
	})
	if err != nil {
		t.Fatalf("UploadSnapshotS3: %v", err)
	}
	for _, forbidden := range []string{"aws", "s3cmd", "AWS_SECRET", "AWS_ACCESS", "--bucket"} {
		assertNotIssued(t, fake, forbidden)
	}
}

// A block source LV that is not on this host fails loud — a backup cannot upload
// bytes that are not there.
func TestUploadSnapshotS3RejectsMissingLV(t *testing.T) {
	fake := newFakeCommands() // snap LV absent

	_, err := UploadSnapshotS3(context.Background(), fake, UploadSnapshotParams{
		SnapshotName: "snap-golden",
		Objects:      []BackupObject{{Name: "rootfs", ObjectName: "rootfs.zst", Source: snapDevice, Block: true, Compress: true, URL: "https://put"}},
	})
	if err == nil {
		t.Fatal("UploadSnapshotS3 accepted a missing source LV")
	}
	assertNotIssued(t, fake, "zstd")
	assertNotIssued(t, fake, "curl")
}

// An empty object plan is a bug, not a silent no-op.
func TestUploadSnapshotS3RejectsEmptyPlan(t *testing.T) {
	fake := newFakeCommands()
	if _, err := UploadSnapshotS3(context.Background(), fake, UploadSnapshotParams{SnapshotName: "x"}); err == nil {
		t.Fatal("UploadSnapshotS3 accepted an empty plan")
	}
	if len(fake.trace) != 0 {
		t.Errorf("touched the host for an empty plan: %v", fake.trace)
	}
}
