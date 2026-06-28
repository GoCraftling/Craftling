//go:build e2e

// End-to-end test for the S3 world store against a real MinIO server started
// via testcontainers. It exercises the same WorldStore contract the DirStore
// unit tests cover, but over the actual S3 protocol (signing, multipart upload,
// not-found mapping).
//
// Run with: go test -tags e2e ./internal/storage/s3/...  (requires Docker).
package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aarani/craftling-go/internal/storage"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
)

func TestS3StoreRoundTrip(t *testing.T) {
	ctx := context.Background()

	ctr, err := tcminio.Run(ctx, "minio/minio:RELEASE.2024-01-16T16-07-38Z")
	if err != nil {
		t.Skipf("start minio container (Docker required): %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctr.Terminate(cctx)
	})

	endpoint, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	const bucket = "worlds"
	// The S3Store requires an existing bucket, so create it up front with a raw
	// client.
	admin, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(ctr.Username, ctr.Password, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("admin client: %v", err)
	}
	if err := admin.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("make bucket: %v", err)
	}

	store, err := New(ctx, Config{
		Endpoint:        endpoint,
		Bucket:          bucket,
		AccessKeyID:     ctr.Username,
		SecretAccessKey: ctr.Password,
		UseSSL:          false,
		Prefix:          "p/",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const id = "srv-1"
	if ok, err := store.Exists(ctx, id); err != nil || ok {
		t.Fatalf("Exists on empty = %v, %v; want false, nil", ok, err)
	}
	if _, err := store.Get(ctx, id); !errors.Is(err, storage.ErrWorldNotFound) {
		t.Fatalf("Get on empty = %v; want ErrWorldNotFound", err)
	}

	payload := bytes.Repeat([]byte("world-snapshot-bytes;"), 1000)
	if err := store.Put(ctx, id, 1, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ok, err := store.Exists(ctx, id); err != nil || !ok {
		t.Fatalf("Exists after Put = %v, %v; want true, nil", ok, err)
	}

	rc, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("round trip mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	// Replace, then verify the new content.
	if err := store.Put(ctx, id, 1, strings.NewReader("newer")); err != nil {
		t.Fatalf("Put replace: %v", err)
	}
	rc, _ = store.Get(ctx, id)
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "newer" {
		t.Errorf("after replace = %q, want newer", got)
	}

	// Generation fence (P8b): claim a higher generation, then a stale Put is
	// rejected without changing the stored world, while the current one still writes.
	if err := store.Claim(ctx, id, 5); err != nil {
		t.Fatalf("Claim gen5: %v", err)
	}
	if err := store.Put(ctx, id, 4, strings.NewReader("stale")); !errors.Is(err, storage.ErrStaleGeneration) {
		t.Fatalf("Put gen4 after claim 5 = %v; want ErrStaleGeneration", err)
	}
	rc, _ = store.Get(ctx, id)
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "newer" {
		t.Errorf("stale Put changed the world: got %q, want newer", got)
	}
	if err := store.Put(ctx, id, 5, strings.NewReader("gen5")); err != nil {
		t.Fatalf("Put gen5: %v", err)
	}

	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := store.Exists(ctx, id); ok {
		t.Error("Exists after Delete = true")
	}
	// Delete is idempotent.
	if err := store.Delete(ctx, id); err != nil {
		t.Errorf("second Delete = %v; want nil", err)
	}
}

// TestS3StoreMissingBucket checks New fails fast against a non-existent bucket.
func TestS3StoreMissingBucket(t *testing.T) {
	ctx := context.Background()
	ctr, err := tcminio.Run(ctx, "minio/minio:RELEASE.2024-01-16T16-07-38Z")
	if err != nil {
		t.Skipf("start minio container (Docker required): %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctr.Terminate(cctx)
	})
	endpoint, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	if _, err := New(ctx, Config{
		Endpoint:        endpoint,
		Bucket:          "does-not-exist",
		AccessKeyID:     ctr.Username,
		SecretAccessKey: ctr.Password,
	}); err == nil {
		t.Fatal("expected New to fail against a missing bucket")
	}
}
