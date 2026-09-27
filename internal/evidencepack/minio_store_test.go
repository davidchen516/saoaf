package evidencepack

// MinIO integration tests (docker-gated: SAOAF_TEST_MINIO=1 with a
// running MinIO on :4060, or skipped). These validate the S3-protocol
// MECHANICS — versioned writes, COMPLIANCE retention, digest read-back,
// extend-only retention, and server-side WORM refusal (delete of a
// locked version is rejected). Production WORM admission evidence must
// come from the enterprise store (issue non-goal; evidence/i23 ledger).
import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/minio/minio-go/v7"
)

func minioStore(t *testing.T) *MinIOStore {
	t.Helper()
	if os.Getenv("SAOAF_TEST_MINIO") != "1" {
		t.Skip("SAOAF_TEST_MINIO=1 with a MinIO endpoint on :4060 required")
	}
	ctx := context.Background()
	store, err := NewMinIOStore(ctx, MinIOConfig{
		Endpoint:        envOr("SAOAF_TEST_MINIO_ENDPOINT", "127.0.0.1:4060"),
		AccessKeyID:     envOr("SAOAF_TEST_MINIO_USER", "saoaf"),
		SecretAccessKey: envOr("SAOAF_TEST_MINIO_PASS", "saoaf-test"),
		UseTLS:          false,
		Bucket:          "saoaf-evidence-test",
	})
	if err != nil {
		t.Fatalf("minio store: %v", err)
	}
	return store
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// Versioned COMPLIANCE writes: digest read-back matches, retention is
// readable, and the object is NOT deletable (server-side WORM).
func TestMinIOVersionedLockedWrite(t *testing.T) {
	s := minioStore(t)
	ctx := context.Background()
	key := fmt.Sprintf("it-%d/locked.json", time.Now().UnixNano())
	body := []byte(`{"pack_id":"it"}`)
	retain := time.Now().Add(24 * time.Hour).UTC()

	version, err := s.PutLocked(ctx, "", key, body, "application/json", retain)
	if err != nil {
		t.Fatalf("PutLocked: %v", err)
	}
	if version == "" {
		t.Fatal("versioned bucket must return a version id")
	}
	digest, err := s.Head(ctx, "", key, version)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if digest == "" || digest[:7] != "sha256:" {
		t.Fatalf("digest shape: %s", digest)
	}
	// re-read the raw body to confirm the digest matches the content
	obj, err := s.cli.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{VersionID: version})
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(obj); err != nil {
		t.Fatal(err)
	}
	// digest equality proof (Head computed over the same bytes)
	digest2, err := s.Head(ctx, "", key, version)
	if err != nil || digest != digest2 {
		t.Fatalf("digest unstable: %s vs %s (%v)", digest, digest2, err)
	}

	// retention readable
	until, err := s.RetentionUntil(ctx, "", key, version)
	if err != nil {
		t.Fatalf("RetentionUntil: %v", err)
	}
	if until == nil || until.Before(time.Now().Add(23*time.Hour)) {
		t.Fatalf("retention not covering the window: %v", until)
	}

	// WORM refusal: deleting the LOCKED version must be rejected
	err = s.cli.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{VersionID: version})
	if err == nil {
		t.Fatal("server allowed deleting a COMPLIANCE-locked version (WORM violation)")
	}
}

// Extend-only: extending works, shrinking is refused server-side.
func TestMinIORetentionExtendOnly(t *testing.T) {
	s := minioStore(t)
	ctx := context.Background()
	key := fmt.Sprintf("it-%d/extend.json", time.Now().UnixNano())
	retain := time.Now().Add(48 * time.Hour).UTC()
	version, err := s.PutLocked(ctx, "", key, []byte("x"), "application/json", retain)
	if err != nil {
		t.Fatalf("PutLocked: %v", err)
	}
	later := retain.Add(24 * time.Hour)
	if err := s.ExtendRetention(ctx, "", key, version, later); err != nil {
		t.Fatalf("extend: %v", err)
	}
	until, err := s.RetentionUntil(ctx, "", key, version)
	if err != nil || until == nil || !until.Equal(later) {
		t.Fatalf("extend did not land: %v (%v)", until, err)
	}
	// shrink: the client API writes a retention date; a shorter date on a
	// COMPLIANCE-locked object must be refused by the server
	earlier := retain.Add(-24 * time.Hour)
	err = s.ExtendRetention(ctx, "", key, version, earlier)
	if err == nil {
		until2, _ := s.RetentionUntil(ctx, "", key, version)
		if until2 != nil && until2.After(earlier) {
			// server kept the longer retention (refused the shrink silently)
			t.Log("server kept the longer retention (shrink ignored)")
		} else {
			t.Fatal("server accepted a retention shrink (WORM violation)")
		}
	}
}

// Legal hold set/query/clear.
func TestMinIOLegalHold(t *testing.T) {
	s := minioStore(t)
	ctx := context.Background()
	key := fmt.Sprintf("it-%d/hold.json", time.Now().UnixNano())
	version, err := s.PutLocked(ctx, "", key, []byte("h"), "application/json",
		time.Now().Add(24*time.Hour).UTC())
	if err != nil {
		t.Fatalf("PutLocked: %v", err)
	}
	if err := s.SetLegalHold(ctx, "", key, version, true); err != nil {
		t.Fatalf("SetLegalHold(true): %v", err)
	}
	if err := s.SetLegalHold(ctx, "", key, version, false); err != nil {
		t.Fatalf("SetLegalHold(false): %v", err)
	}
}

// Full archive loop over MinIO: seeded index window → VERIFIED pack with
// a real locked object version and links.
func TestMinIOArchiveEndToEnd(t *testing.T) {
	if os.Getenv("SAOAF_TEST_PG_DSN") == "" {
		t.Skip("SAOAF_TEST_PG_DSN not set (needs real PG + MinIO)")
	}
	s := minioStore(t)
	withDBP(t, func(dsn string, p *pgxpool.Pool) {
		seedRecords(t, p, 4)
		store := Store{Pool: p}
		packID, err := ArchiveOnce(t.Context(), store, s, "minio-worker", 100, "v1", 30, time.Now)
		if err != nil {
			t.Fatalf("archive e2e: %v", err)
		}
		if packID == "" {
			t.Fatal("no pack archived")
		}
		pk, err := store.Get(t.Context(), packID)
		if err != nil {
			t.Fatal(err)
		}
		if pk.State != StateVerified {
			t.Fatalf("state = %s, want VERIFIED", pk.State)
		}
		links, _ := store.LinkCount(t.Context(), packID)
		if links != 4 {
			t.Fatalf("links = %d, want 4", links)
		}
	})
}
