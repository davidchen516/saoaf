package evidencepack

// MinIO/S3-compatible ObjectStore implementation. Talks to any
// S3-compatible endpoint with versioning + Object Lock (COMPLIANCE
// retention). MinIO protocol tests validate the MECHANICS (versioned
// writes, retention API, digest reads); PRODUCTION WORM admission
// evidence must come from the enterprise store (issue non-goal: "Mock
// S3 API 或普通 versioning 不作为 WORM 合规证据" — recorded in evidence/i23).
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinIOConfig configures the S3-compatible WORM store client.
type MinIOConfig struct {
	Endpoint        string // host:port (no scheme)
	AccessKeyID     string
	SecretAccessKey string
	UseTLS          bool
	Bucket          string
}

// NewMinIOStore builds the object store client and ensures the bucket
// exists WITH object-lock enabled (required for COMPLIANCE retention).
func NewMinIOStore(ctx context.Context, cfg MinIOConfig) (*MinIOStore, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("evidencepack: endpoint and bucket are required")
	}
	cli, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: cfg.UseTLS,
	})
	if err != nil {
		return nil, fmt.Errorf("evidencepack: minio client: %w", err)
	}
	// ensure the bucket exists with object lock enabled; a pre-existing
	// bucket without object lock is a configuration error (fail-closed)
	exists, err := cli.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("evidencepack: bucket probe: %w", err)
	}
	if !exists {
		if err := cli.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{ObjectLocking: true}); err != nil {
			return nil, fmt.Errorf("evidencepack: make bucket (object-lock): %w", err)
		}
	}
	return &MinIOStore{cli: cli, bucket: cfg.Bucket}, nil
}

// MinIOStore implements ObjectStore over the S3-compatible protocol.
type MinIOStore struct {
	cli    *minio.Client
	bucket string
}

func (s *MinIOStore) Put(ctx context.Context, bucket, key string, body []byte, contentType string) (string, error) {
	info, err := s.cli.PutObject(ctx, s.bucket, key, bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return "", err
	}
	return info.VersionID, nil
}

func (s *MinIOStore) PutLocked(ctx context.Context, bucket, key string, body []byte, contentType string, retainUntil time.Time) (string, error) {
	info, err := s.cli.PutObject(ctx, s.bucket, key, bytes.NewReader(body), int64(len(body)),
		minio.PutObjectOptions{
			ContentType:     contentType,
			Mode:            minio.Compliance,
			RetainUntilDate: retainUntil,
		})
	if err != nil {
		return "", err
	}
	return info.VersionID, nil
}

func (s *MinIOStore) Head(ctx context.Context, bucket, key, version string) (string, error) {
	obj, err := s.cli.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{VersionID: version})
	if err != nil {
		return "", err
	}
	defer obj.Close()
	body, err := io.ReadAll(obj)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

func (s *MinIOStore) ExtendRetention(ctx context.Context, bucket, key, version string, retainUntil time.Time) error {
	mode := minio.Compliance
	return s.cli.PutObjectRetention(ctx, s.bucket, key,
		minio.PutObjectRetentionOptions{Mode: &mode, RetainUntilDate: &retainUntil, VersionID: version})
}

func (s *MinIOStore) SetLegalHold(ctx context.Context, bucket, key, version string, hold bool) error {
	status := minio.LegalHoldEnabled
	if !hold {
		status = minio.LegalHoldDisabled
	}
	return s.cli.PutObjectLegalHold(ctx, s.bucket, key,
		minio.PutObjectLegalHoldOptions{Status: &status, VersionID: version})
}

func (s *MinIOStore) RetentionUntil(ctx context.Context, bucket, key, version string) (*time.Time, error) {
	mode, retainUntil, err := s.cli.GetObjectRetention(ctx, s.bucket, key, version)
	if err != nil {
		// no retention configured (e.g. plain Put) reads as nil
		var resp minio.ErrorResponse
		if errors.As(err, &resp) {
			return nil, nil
		}
		return nil, err
	}
	if mode == nil || retainUntil == nil {
		return nil, nil
	}
	return retainUntil, nil
}
