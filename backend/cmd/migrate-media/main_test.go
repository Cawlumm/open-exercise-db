package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cawlumm/open-exercise-db/internal/media"
)

// The defect this file exists for: `run` reported (copied, failed) and main
// exited non-zero only when failed > 0, but an interrupted run abandons keys
// that no worker ever touches — nothing increments failed for them. A Ctrl-C'd
// backfill that copied 500 of 1,700 objects therefore printed "0 failed" and
// exited 0, which is enough for `migrate-media && <cut over to R2>` to proceed
// onto a bucket missing two thirds of its images, with no record of which run
// dropped them. Success is now "copied == len(keys)", derived from what was
// asked for rather than from what happened to be noticed.

// fakeStore is an in-memory media.Store. Get/Put may be made to fail per key.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	failOn  map[string]error
	// beforePut runs before each Put, letting a test cancel the context
	// mid-run at a deterministic point rather than racing a timer.
	beforePut func(key string)
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}, failOn: map[string]error{}}
}

func (f *fakeStore) Put(ctx context.Context, key string, r io.Reader, _ string) error {
	if f.beforePut != nil {
		f.beforePut(key)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	err := f.failOn[key]
	f.mu.Unlock()
	if err != nil {
		return err
	}

	data, readErr := io.ReadAll(r)
	if readErr != nil {
		return readErr
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
	return nil
}

func (f *fakeStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, media.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Exists mirrors Get's view of the map rather than tracking presence
// separately, so a test that seeds an object through Put or removes one
// cannot leave the two answers disagreeing.
func (f *fakeStore) Exists(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOn[key]; err != nil {
		return false, err
	}
	_, ok := f.objects[key]
	return ok, nil
}

func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.objects)
}

func sourceWith(keys ...string) *fakeStore {
	src := newFakeStore()
	for _, k := range keys {
		src.objects[k] = []byte("bytes-of-" + k)
	}
	return src
}

func TestRun_CopiesEverything(t *testing.T) {
	keys := []string{"exercises/a/0.jpg", "exercises/b/0.jpg", "exercises/c/0.jpg"}
	src, dst := sourceWith(keys...), newFakeStore()

	copied, failed, interrupted := run(context.Background(), src, dst, keys, 2)

	if copied != int64(len(keys)) || failed != 0 || interrupted {
		t.Errorf("run() = (%d, %d, %v), want (%d, 0, false)", copied, failed, interrupted, len(keys))
	}
	if dst.count() != len(keys) {
		t.Errorf("destination holds %d objects, want %d", dst.count(), len(keys))
	}
}

// TestRun_InterruptedReportsShortfall is the regression. It asserts on the
// relationship between copied and len(keys) — the thing main's exit code is
// now derived from — rather than on `failed`, which is exactly the counter
// that stayed at zero while two thirds of the dataset went missing.
func TestRun_InterruptedReportsShortfall(t *testing.T) {
	keys := []string{}
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		keys = append(keys, "exercises/"+name+"/0.jpg")
	}
	src, dst := sourceWith(keys...), newFakeStore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel partway through, the way SIGINT would. Single worker so the
	// cancellation point is deterministic instead of racing the pool.
	var puts int
	dst.beforePut = func(string) {
		puts++
		if puts == 3 {
			cancel()
		}
	}

	copied, failed, interrupted := run(ctx, src, dst, keys, 1)

	if !interrupted {
		t.Error("interrupted = false, want true after context cancellation")
	}
	if copied >= int64(len(keys)) {
		t.Fatalf("copied = %d of %d, want strictly fewer — the run was cancelled", copied, len(keys))
	}
	// The whole point: this used to be the only signal main looked at, and it
	// is legitimately zero here. Nothing failed; work was simply abandoned.
	if failed != 0 {
		t.Logf("failed = %d (not required to be zero, recorded for context)", failed)
	}
	if missing := int64(len(keys)) - copied; missing <= 0 {
		t.Errorf("shortfall = %d, want > 0 so main exits non-zero", missing)
	}
}

// TestRun_FailuresAreCountedAndNonFatal pins that a single bad object does not
// abandon the rest — the property that makes re-running safe and cheap.
func TestRun_FailuresAreCountedAndNonFatal(t *testing.T) {
	keys := []string{"exercises/a/0.jpg", "exercises/b/0.jpg", "exercises/c/0.jpg"}
	src, dst := sourceWith(keys...), newFakeStore()
	dst.failOn["exercises/b/0.jpg"] = errors.New("transient 500 from object storage")

	copied, failed, interrupted := run(context.Background(), src, dst, keys, 2)

	if interrupted {
		t.Error("interrupted = true, want false — nothing cancelled this run")
	}
	if copied != 2 || failed != 1 {
		t.Errorf("run() = (%d copied, %d failed), want (2, 1)", copied, failed)
	}
	if missing := int64(len(keys)) - copied; missing != 1 {
		t.Errorf("shortfall = %d, want 1 so main exits non-zero", missing)
	}
}

// TestShortfall_DrivesTheExitCode pins the exact rule that was wrong. The old
// condition was `failed > 0`; the "interrupted" row below is the case that
// made it wrong, and it is the row that would have caught the defect. Note
// that `failed` does not appear in the rule at all — a failed object is
// already an uncopied one, so counting it twice is what let the two diverge.
func TestShortfall_DrivesTheExitCode(t *testing.T) {
	cases := []struct {
		name     string
		copied   int64
		total    int
		wantExit bool
	}{
		{"everything copied", 1700, 1700, false},
		{"nothing to do", 0, 0, false},
		{"some failed", 1699, 1700, true},
		// The regression: no worker reported a failure, yet 1,200 objects were
		// never attempted. Under `failed > 0` this exited 0.
		{"interrupted after 500 of 1700", 500, 1700, true},
		{"interrupted immediately", 0, 1700, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if gotExit := shortfall(c.copied, c.total) > 0; gotExit != c.wantExit {
				t.Errorf("shortfall(%d, %d) > 0 = %v, want %v", c.copied, c.total, gotExit, c.wantExit)
			}
		})
	}
}

// TestCollectKeys_UsesSlashSeparators guards the Windows-authored/Linux-served
// corruption: storage keys are opaque strings, not paths, so a backslash would
// write a key no deployed instance could ever look up.
func TestCollectKeys_UsesSlashSeparators(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "exercises/Ab_Roller/0.jpg", "image")
	writeFile(t, root, "exercises/Ab_Roller/1.jpg", "image")
	writeFile(t, root, "exercises/Bench/0.jpg.tmp", "half-written")

	keys, err := collectKeys(root)
	if err != nil {
		t.Fatalf("collectKeys: %v", err)
	}

	for _, k := range keys {
		if strings.Contains(k, `\`) {
			t.Errorf("key %q contains a backslash; keys must be slash-separated on every platform", k)
		}
		if strings.HasSuffix(k, ".tmp") {
			t.Errorf("key %q is an interrupted-write temp file and should have been skipped", k)
		}
	}
	if len(keys) != 2 {
		t.Errorf("collected %d keys (%v), want 2", len(keys), keys)
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
