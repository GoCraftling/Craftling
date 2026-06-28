// Package s3 is an S3-compatible storage.WorldStore (P5b). It lives apart from
// internal/storage so the lightweight interface (which the Firecracker driver
// imports) doesn't drag the S3 SDK into every build that only needs the
// abstraction — only cmd/agent, which constructs a concrete store, pulls it in.
//
// It works against AWS S3 and any S3-compatible service (MinIO, Ceph RGW,
// Backblaze B2, …) via minio-go. Objects are keyed
// "<prefix><safe-server-id>.world"; the bytes are opaque (the agent gzips a raw
// ext4 image into them), matching the DirStore so the two are interchangeable.
package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/aarani/craftling-go/internal/storage"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Config configures an S3Store.
type Config struct {
	// Endpoint is the S3 host:port (e.g. "s3.amazonaws.com",
	// "minio.internal:9000"). Required.
	Endpoint string
	// Bucket holds the world snapshots. Required; must already exist (the
	// agent is not granted bucket-creation rights in normal deployments).
	Bucket string
	// AccessKeyID / SecretAccessKey authenticate to the service. Empty falls
	// back to the AWS credential chain (env, IAM role) via minio-go's IAM
	// provider, so an EC2/EKS agent can use an instance/role identity.
	AccessKeyID     string
	SecretAccessKey string
	// Region is the bucket region (e.g. "us-east-1"). Optional for MinIO.
	Region string
	// UseSSL switches between https and http. Default (false) is plain http,
	// suitable for an in-cluster MinIO; set true for AWS S3.
	UseSSL bool
	// Prefix is prepended to every object key (e.g. "worlds/"), so one bucket
	// can hold worlds alongside other data. Optional.
	Prefix string
}

// S3Store implements storage.WorldStore over an S3-compatible bucket.
type S3Store struct {
	client *minio.Client
	bucket string
	prefix string
}

// compile-time check.
var _ storage.WorldStore = (*S3Store)(nil)

// New constructs an S3Store and verifies the bucket is reachable, so a
// misconfigured endpoint/credential/bucket fails at agent startup rather than
// at the first snapshot.
func New(ctx context.Context, cfg Config) (*S3Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, fmt.Errorf("storage/s3: Endpoint and Bucket are required")
	}

	var creds *credentials.Credentials
	if cfg.AccessKeyID != "" {
		creds = credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, "")
	} else {
		// No static keys: use the AWS credential chain (env vars, then IAM).
		creds = credentials.NewIAM("")
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  creds,
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("storage/s3: client: %w", err)
	}

	ok, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("storage/s3: reach bucket %q: %w", cfg.Bucket, err)
	}
	if !ok {
		return nil, fmt.Errorf("storage/s3: bucket %q does not exist", cfg.Bucket)
	}

	return &S3Store{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

// key derives the snapshot object key for a server id.
func (s *S3Store) key(serverID string) string {
	return s.prefix + storage.SafeKey(serverID) + storage.WorldSuffix
}

// genKey derives the generation-watermark object key for a server id.
func (s *S3Store) genKey(serverID string) string {
	return s.prefix + storage.SafeKey(serverID) + storage.GenSuffix
}

// readGen returns the recorded generation watermark for serverID, or 0 when no
// watermark object exists yet.
func (s *S3Store) readGen(ctx context.Context, serverID string) (int64, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.genKey(serverID), minio.GetObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("storage/s3: get generation %q: %w", serverID, err)
	}
	defer func() { _ = obj.Close() }()
	b, err := io.ReadAll(obj)
	if err != nil {
		if isNotFound(err) {
			return 0, nil // lazy GetObject surfaces a missing object on read
		}
		return 0, fmt.Errorf("storage/s3: read generation %q: %w", serverID, err)
	}
	g, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("storage/s3: parse generation %q: %w", serverID, err)
	}
	return g, nil
}

// writeGen records the generation watermark for serverID.
func (s *S3Store) writeGen(ctx context.Context, serverID string, generation int64) error {
	body := strconv.FormatInt(generation, 10)
	_, err := s.client.PutObject(ctx, s.bucket, s.genKey(serverID),
		strings.NewReader(body), int64(len(body)), minio.PutObjectOptions{ContentType: "text/plain"})
	if err != nil {
		return fmt.Errorf("storage/s3: put generation %q: %w", serverID, err)
	}
	return nil
}

// fence returns ErrStaleGeneration when generation is older than the recorded
// watermark for serverID. As with the DirStore the read-compare-write is not
// atomic, but only one live VM owns a server and a fenced zombie is always a
// strictly lower generation, so the realistic race window is nil.
func (s *S3Store) fence(ctx context.Context, serverID string, generation int64) error {
	current, err := s.readGen(ctx, serverID)
	if err != nil {
		return err
	}
	if generation < current {
		return storage.ErrStaleGeneration
	}
	return nil
}

// Claim raises the generation watermark for serverID without writing a snapshot.
func (s *S3Store) Claim(ctx context.Context, serverID string, generation int64) error {
	if err := s.fence(ctx, serverID, generation); err != nil {
		return err
	}
	return s.writeGen(ctx, serverID, generation)
}

// Exists reports whether a snapshot object is present for serverID.
func (s *S3Store) Exists(ctx context.Context, serverID string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, s.key(serverID), minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("storage/s3: stat world %q: %w", serverID, err)
}

// Put streams r to the snapshot object, replacing any prior one. The size is
// unknown ahead of time (the gzip stream), so we pass -1, which makes minio-go
// upload via multipart; a failure aborts the upload rather than leaving a
// truncated object.
func (s *S3Store) Put(ctx context.Context, serverID string, generation int64, r io.Reader) error {
	if err := s.fence(ctx, serverID, generation); err != nil {
		return err
	}
	_, err := s.client.PutObject(ctx, s.bucket, s.key(serverID), r, -1, minio.PutObjectOptions{
		ContentType: "application/gzip",
	})
	if err != nil {
		return fmt.Errorf("storage/s3: put world %q: %w", serverID, err)
	}
	if err := s.writeGen(ctx, serverID, generation); err != nil {
		return err
	}
	return nil
}

// Get opens the snapshot object, mapping a missing object to ErrWorldNotFound.
// minio-go's GetObject is lazy (errors surface on first read), so we Stat first
// to detect a missing object eagerly and return the sentinel before any bytes
// are read.
func (s *S3Store) Get(ctx context.Context, serverID string) (io.ReadCloser, error) {
	key := s.key(serverID)
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		if isNotFound(err) {
			return nil, storage.ErrWorldNotFound
		}
		return nil, fmt.Errorf("storage/s3: stat world %q: %w", serverID, err)
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("storage/s3: get world %q: %w", serverID, err)
	}
	return obj, nil
}

// Delete removes the snapshot object and its generation watermark; a missing
// object is success.
func (s *S3Store) Delete(ctx context.Context, serverID string) error {
	err := s.client.RemoveObject(ctx, s.bucket, s.key(serverID), minio.RemoveObjectOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("storage/s3: delete world %q: %w", serverID, err)
	}
	gerr := s.client.RemoveObject(ctx, s.bucket, s.genKey(serverID), minio.RemoveObjectOptions{})
	if gerr != nil && !isNotFound(gerr) {
		return fmt.Errorf("storage/s3: delete generation %q: %w", serverID, gerr)
	}
	return nil
}

// List returns the stored snapshot keys (object names with the prefix and
// suffix stripped). Objects that don't fit the "<prefix>…<suffix>" shape are
// skipped, so an unrelated object sharing the bucket is never reported.
func (s *S3Store) List(ctx context.Context) ([]string, error) {
	var keys []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("storage/s3: list worlds: %w", obj.Err)
		}
		name := strings.TrimPrefix(obj.Key, s.prefix)
		if !strings.HasSuffix(name, storage.WorldSuffix) {
			continue
		}
		keys = append(keys, strings.TrimSuffix(name, storage.WorldSuffix))
	}
	return keys, nil
}

// isNotFound reports whether an error from minio-go means the object (or bucket)
// is absent. minio-go surfaces these as a typed ErrorResponse with an S3 code or
// a 404 status.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case "NoSuchKey", "NoSuchBucket", "NotFound":
		return true
	}
	if resp.StatusCode == http.StatusNotFound {
		return true
	}
	// Some paths wrap os.ErrNotExist-like errors; be lenient on the message.
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}
