// Command migrate-media copies every re-hosted image from the local
// filesystem media directory into the configured object-storage bucket,
// preserving storage keys exactly.
//
// Keys are preserved deliberately: exercise_images.storage_key is what both
// internal/api's image route and internal/export resolve URLs from, so a
// copy that keeps keys identical needs no database migration and no
// coordinated cutover. Point IMAGE_BASE_URL (or the R2 credentials) at the
// new backend and the same rows keep resolving — the two backends are
// interchangeable precisely because media.ValidateKey gives them one shared
// key space.
//
// Idempotent by construction rather than by bookkeeping: every object is
// re-uploaded on every run, and object storage is last-write-wins (pinned by
// TestR2Store_OverwriteIsLastWriteWins). A resumed or repeated run is
// therefore always safe, at the cost of re-sending bytes that are already
// there. For this dataset that trade is obviously right — ~1,700 objects
// averaging tens of KB, against a 1M/month Class A operation allowance — and
// it avoids both a HeadObject round-trip per key and a Head method on
// media.Store that nothing else would use.
//
// Usage:
//
//	go run ./cmd/migrate-media            # copy MEDIA_DIR -> R2
//	go run ./cmd/migrate-media -dry-run   # list what would be copied
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Cawlumm/open-exercise-db/internal/config"
	"github.com/Cawlumm/open-exercise-db/internal/media"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "list the keys that would be copied without uploading")
	workers := flag.Int("workers", 8, "number of concurrent uploads")
	flag.Parse()

	cfg := config.Load()

	if !cfg.UsesR2() {
		log.Fatal("object storage is not configured: set R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, " +
			"R2_SECRET_ACCESS_KEY and R2_BUCKET (there is nothing to migrate to otherwise)")
	}
	if *workers < 1 {
		log.Fatalf("-workers must be at least 1, got %d", *workers)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Source is always the filesystem: this command exists to get bytes *off*
	// local disk. Constructed directly rather than through media.NewStore,
	// which would select R2 here and make the copy a no-op against itself.
	src, err := media.NewFilesystemStore(cfg.MediaDir)
	if err != nil {
		log.Fatalf("open source media directory %s: %v", cfg.MediaDir, err)
	}

	dst, err := media.NewR2Store(
		cfg.R2AccountID, cfg.R2AccessKeyID, cfg.R2SecretAccessKey, cfg.R2Bucket,
	)
	if err != nil {
		log.Fatalf("init destination: %v", err)
	}

	keys, err := collectKeys(cfg.MediaDir)
	if err != nil {
		log.Fatalf("scan %s: %v", cfg.MediaDir, err)
	}
	if len(keys) == 0 {
		log.Fatalf("no files found under %s — is MEDIA_DIR pointing at the right place?", cfg.MediaDir)
	}

	log.Printf("copying %d objects: %s -> r2 bucket %q", len(keys), cfg.MediaDir, cfg.R2Bucket)
	if *dryRun {
		for _, k := range keys {
			fmt.Println(k)
		}
		log.Printf("dry run: %d objects would be copied, nothing uploaded", len(keys))
		return
	}

	start := time.Now()
	copied, failed, interrupted := run(ctx, src, dst, keys, *workers)

	// The success condition is "every key made it", not "nothing reported an
	// error". Those are not the same: an interrupted run abandons keys that
	// were never fed to a worker and cancels uploads already in flight, and
	// neither of those is a failure any worker observes. Keying the exit code
	// off `failed` alone let a Ctrl-C'd run that copied 500 of 1,700 objects
	// print "0 failed" and exit 0 — enough for `migrate-media && <cut over to
	// R2>` to proceed onto a bucket missing two thirds of its images. Deriving
	// the shortfall from len(keys) instead cannot drift, because it counts what
	// was asked for rather than what happened to be noticed.
	log.Printf("done in %s: %d/%d copied, %d failed", time.Since(start).Round(time.Millisecond), copied, len(keys), failed)
	if missing := shortfall(copied, len(keys)); missing > 0 {
		if interrupted {
			log.Printf("interrupted: %d objects were not copied — re-run to finish (re-running is safe)", missing)
		} else {
			log.Printf("%d objects were not copied — re-run to retry (re-running is safe)", missing)
		}
		os.Exit(1)
	}
}

// shortfall is how many of total never made it. Extracted from main purely so
// it is testable: the defect this replaced lived in main's exit condition
// (`if failed > 0`), which no test could reach, and so a rule that was wrong
// for the entire interrupted-run case looked fine in review and in CI.
func shortfall(copied int64, total int) int64 {
	return int64(total) - copied
}

// collectKeys walks root and returns every regular file as a storage key
// relative to root, slash-separated.
//
// Slash-separated regardless of platform because storage keys are not paths:
// they are opaque strings stored in exercise_images.storage_key and used
// verbatim as object names. Walking on Windows yields backslashes, and
// letting those through would write keys like `exercises\Ab_Roller\0.jpg`
// that no Linux-hosted API instance could ever look up — a corruption that
// would only surface after deploy, on someone else's machine.
func collectKeys(root string) ([]string, error) {
	var keys []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Skip FilesystemStore.Put's interrupted-write temp files rather than
		// faithfully copying a half-written image into object storage under a
		// key nothing will ever read.
		if strings.HasSuffix(path, ".tmp") {
			log.Printf("skipping temp file %s", path)
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relative path for %s: %w", path, err)
		}
		key := filepath.ToSlash(rel)

		// The same validation both stores apply, run here so a bad key is
		// reported once with its full path rather than as N opaque upload
		// failures.
		if err := media.ValidateKey(key); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// run copies keys from src to dst using workers concurrent uploads. It
// returns how many succeeded, how many failed outright, and whether the run
// was cut short by context cancellation.
//
// A failed object is logged and counted, never fatal: one unreadable file or
// one transient 5xx should not abandon the other 1,700 objects when the whole
// command is safe to re-run. The non-zero exit in main is what stops a
// partial copy from being mistaken for a complete one — and it compares
// copied against len(keys) rather than trusting failed, precisely because
// interruption skips keys without any worker ever counting them.
func run(ctx context.Context, src, dst media.Store, keys []string, workers int) (copied, failed int64, interrupted bool) {
	if workers > len(keys) {
		workers = len(keys)
	}

	work := make(chan string)
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range work {
				if err := copyOne(ctx, src, dst, key); err != nil {
					if errors.Is(err, context.Canceled) {
						return
					}
					log.Printf("copy %s: %v", key, err)
					atomic.AddInt64(&failed, 1)
					continue
				}
				if n := atomic.AddInt64(&copied, 1); n%200 == 0 {
					log.Printf("  %d/%d copied", n, len(keys))
				}
			}
		}()
	}

	for _, key := range keys {
		select {
		case work <- key:
		case <-ctx.Done():
			// Stop feeding on Ctrl-C. Every remaining key is silently dropped
			// here — no worker sees it, so nothing increments failed — which is
			// exactly why the interrupted flag travels back to main rather than
			// letting the counts speak for themselves.
			close(work)
			wg.Wait()
			return atomic.LoadInt64(&copied), atomic.LoadInt64(&failed), true
		}
	}
	close(work)
	wg.Wait()

	return atomic.LoadInt64(&copied), atomic.LoadInt64(&failed), ctx.Err() != nil
}

// copyOne streams one object from src to dst.
//
// contentType is passed empty: FilesystemStore has never persisted it (the
// exercise_images.content_type column is the source of truth — see
// FilesystemStore.Put), so there is nothing on disk to read it back from,
// and inferring it from the file extension is exactly the guess this
// project's image handling avoids. R2Store.Put omits the header entirely
// when it is empty, leaving the object without a stored Content-Type; the
// API supplies the real one from the database on every response. Setting it
// on the objects themselves only matters once IMAGE_BASE_URL points clients
// straight at the bucket, which is Phase 2 and wants a re-upload pass with
// the database values anyway.
func copyOne(ctx context.Context, src, dst media.Store, key string) error {
	rc, err := src.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()

	return dst.Put(ctx, key, rc, "")
}
