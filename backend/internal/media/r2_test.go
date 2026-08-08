package media_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Cawlumm/open-exercise-db/internal/media"
)

// requireR2Env, when set to any non-empty value, turns missing R2
// credentials from a quiet skip into a hard test failure — the same shape as
// internal/database's OEDB_REQUIRE_POSTGRES, and for the same reason
// (docs/CONVENTIONS.md section 6). Without it these tests skip, so a
// contributor with no Cloudflare account still gets a green `go test ./...`;
// with it, an environment that is *supposed* to have R2 configured cannot
// report green by silently skipping every call that would have touched it.
const requireR2Env = "OEDB_REQUIRE_R2"

// r2TestKeyPrefix namespaces every object these tests write. Kept distinct
// from the "exercises/" prefix real data uses so a stray test object can
// never be mistaken for — or collide with — a re-hosted image, and so the
// whole prefix can be deleted wholesale if one ever leaks past cleanup.
const r2TestKeyPrefix = "test-objects/"

func skipOrFailR2(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv(requireR2Env) != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// requireR2 builds an R2Store from the same environment variables
// internal/config reads, skipping (or failing, per requireR2Env) when they
// are not all present. It does not mock: these tests either talk to a real
// bucket or do not run.
func requireR2(t *testing.T) media.Store {
	t.Helper()

	opts := media.StoreOptions{
		R2AccountID:       os.Getenv("R2_ACCOUNT_ID"),
		R2AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		R2SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		R2Bucket:          os.Getenv("R2_BUCKET"),
	}
	if opts.R2AccountID == "" || opts.R2AccessKeyID == "" ||
		opts.R2SecretAccessKey == "" || opts.R2Bucket == "" {
		skipOrFailR2(t, "R2 not configured (need R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, "+
			"R2_SECRET_ACCESS_KEY, R2_BUCKET)")
	}

	s, desc, err := media.NewStore(opts)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if !strings.HasPrefix(desc, "r2 ") {
		t.Fatalf("NewStore selected %q, want an r2 backend", desc)
	}
	return s
}

// testKey returns a key unique to this test run. Uniqueness matters because
// R2 is shared mutable state: two `go test` runs in parallel (or a rerun
// after a failure left an object behind) would otherwise read each other's
// bytes and pass for the wrong reason.
func testKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s%s-%d.bin", r2TestKeyPrefix, t.Name(), time.Now().UnixNano())
}

func TestR2Store_PutThenGet(t *testing.T) {
	s := requireR2(t)
	ctx := context.Background()
	key := testKey(t)

	// Deliberately non-UTF8 bytes: images are binary, and a store that
	// silently transcodes or truncates would still pass a plain-ASCII
	// round-trip.
	want := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}

	if err := s.Put(ctx, key, bytes.NewReader(want), "image/jpeg"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("round-trip mismatch:\n got %x\nwant %x", got, want)
	}
}

func TestR2Store_GetMissingKeyIsErrObjectNotFound(t *testing.T) {
	s := requireR2(t)

	// Not merely "an error": the images route and cmd/migrate-media both need
	// to tell a never-written object apart from a credential or network
	// failure, so the specific sentinel is the contract being tested.
	_, err := s.Get(context.Background(), testKey(t))
	if err == nil {
		t.Fatal("Get on a missing key returned nil error")
	}
	if !errors.Is(err, media.ErrObjectNotFound) {
		t.Errorf("Get on a missing key = %v, want it to wrap media.ErrObjectNotFound", err)
	}
}

// TestR2Store_Exists covers the check internal/seed's repair path runs once
// per image to tell a live object from an exercise_images row that outlived
// its bytes.
//
// This one genuinely needs a real bucket rather than a fake. S3 answers a
// missing GetObject with NoSuchKey but a missing HeadObject with NotFound —
// a HEAD response has no body to carry the richer code — and
// implementations differ on which they mirror. A mock would return whichever
// error the mock's author chose, which is precisely the assumption under
// test.
func TestR2Store_Exists(t *testing.T) {
	s := requireR2(t)
	ctx := context.Background()
	key := testKey(t)

	// Absent must be (false, nil): a repair that read "missing" as an error
	// would skip forever, and one that read an error as "missing" would
	// re-upload the whole bucket on a transient failure.
	ok, err := s.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists on a missing key returned an error: %v — absence is an answer, not a failure", err)
	}
	if ok {
		t.Error("Exists = true for a key that was never written")
	}

	if err := s.Put(ctx, key, strings.NewReader("bytes"), "application/octet-stream"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	ok, err = s.Exists(ctx, key)
	if err != nil {
		t.Fatalf("Exists after Put: %v", err)
	}
	if !ok {
		t.Error("Exists = false for a key that was just written")
	}

	if _, err := s.Exists(ctx, "../escape.bin"); err == nil {
		t.Error("Exists accepted a traversal key that Put and Get reject")
	}
}

// TestR2Store_OverwriteIsLastWriteWins pins the behavior cmd/migrate-media
// depends on for idempotency: re-running a backfill over an existing object
// must succeed and leave the newer bytes, not error on a conflict.
func TestR2Store_OverwriteIsLastWriteWins(t *testing.T) {
	s := requireR2(t)
	ctx := context.Background()
	key := testKey(t)

	if err := s.Put(ctx, key, strings.NewReader("first"), "text/plain"); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := s.Put(ctx, key, strings.NewReader("second"), "text/plain"); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("after overwrite got %q, want %q", got, "second")
	}
}

func TestR2Store_RejectsInvalidKeys(t *testing.T) {
	s := requireR2(t)
	ctx := context.Background()

	// Same key space as FilesystemStore (media.ValidateKey) — asserted here
	// rather than only in filesystem_test.go, because a backend that accepted
	// keys the other rejects would break the "same key works on both"
	// property cmd/migrate-media relies on.
	for _, key := range []string{"", "../escape.jpg", "exercises/../../etc/passwd"} {
		t.Run(fmt.Sprintf("key=%q", key), func(t *testing.T) {
			if err := s.Put(ctx, key, strings.NewReader("x"), "text/plain"); err == nil {
				t.Errorf("Put(%q) succeeded, want rejection", key)
			}
			if _, err := s.Get(ctx, key); err == nil {
				t.Errorf("Get(%q) succeeded, want rejection", key)
			}
		})
	}
}

func TestNewStore_PartialR2ConfigIsAnError(t *testing.T) {
	// No credentials needed — this is pure selection logic, so it runs
	// everywhere, including for contributors with no Cloudflare account.
	_, _, err := media.NewStore(media.StoreOptions{
		R2AccountID: "abc123",
		MediaDir:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("NewStore with 1 of 4 R2 settings succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "partially configured") {
		t.Errorf("error = %v, want it to say the config is partial", err)
	}
}

func TestNewStore_NoR2SelectsFilesystem(t *testing.T) {
	dir := t.TempDir()
	s, desc, err := media.NewStore(media.StoreOptions{MediaDir: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if !strings.HasPrefix(desc, "filesystem ") {
		t.Errorf("desc = %q, want a filesystem backend", desc)
	}
	if _, ok := s.(*media.FilesystemStore); !ok {
		t.Errorf("got %T, want *media.FilesystemStore", s)
	}
}
