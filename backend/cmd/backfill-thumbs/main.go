// Command backfill-thumbs gives every existing image a thumb rendition and
// records the content hash of its full rendition.
//
// New images get both at write time (internal/seed, the upload route). This
// is for the ones written before that: it reads each image's full bytes
// from the media store, hashes them into the full variant row (creating the
// row if the image has none), encodes a thumb with media.EncodeThumb, writes
// it next to the full object, and records the thumb's variant row.
//
// Resumable and safe to run twice by construction rather than bookkeeping:
// it only ever selects images that still lack a thumb or a recorded full
// hash (store.MediaStore.ListImagesNeedingThumb), and every write is an
// upsert or an overwrite of a deterministic key. An interrupted run leaves
// at worst a thumb object with no row, which the next run overwrites.
//
// Usage:
//
//	go run ./cmd/backfill-thumbs -dry-run          # list what would be done
//	go run ./cmd/backfill-thumbs -limit 50         # do the first 50
//	go run ./cmd/backfill-thumbs                   # do all of them
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Cawlumm/open-exercise-db/internal/config"
	"github.com/Cawlumm/open-exercise-db/internal/database"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/model"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "list the images that need work without reading or writing anything")
	limit := flag.Int("limit", 0, "process at most N images (0 = no limit)")
	batch := flag.Int("batch", 100, "images fetched from the database per query")
	flag.Parse()

	if *batch < 1 {
		log.Fatalf("-batch must be at least 1, got %d", *batch)
	}
	if *limit < 0 {
		log.Fatalf("-limit must not be negative, got %d", *limit)
	}

	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer pool.Close()

	// Migrated here because the content_sha256 column this writes arrives
	// in migration 0005; the same call every other command makes.
	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate database: %v", err)
	}

	var mediaStore media.Store
	if !*dryRun {
		// Same selection as cmd/api-server, so the thumbs land on the backend
		// that serves them. Logged rather than assumed.
		ms, desc, err := media.NewStore(media.StoreOptions{
			R2AccountID:       cfg.R2AccountID,
			R2AccessKeyID:     cfg.R2AccessKeyID,
			R2SecretAccessKey: cfg.R2SecretAccessKey,
			R2Bucket:          cfg.R2Bucket,
			MediaDir:          cfg.MediaDir,
		})
		if err != nil {
			log.Fatalf("init media store: %v", err)
		}
		log.Printf("media store: %s", desc)
		mediaStore = ms
	}

	start := time.Now()
	res, err := run(ctx, store.New(pool).Media, mediaStore, options{
		dryRun: *dryRun,
		limit:  *limit,
		batch:  *batch,
		out:    os.Stdout,
	})
	if err != nil {
		log.Fatalf("backfill: %v", err)
	}

	if *dryRun {
		log.Printf("dry run: %d image(s) need a thumb or a full-rendition hash; nothing read or written", res.seen)
		return
	}
	log.Printf("done in %s: %d processed, %d failed", time.Since(start).Round(time.Millisecond), res.done, res.failed)
	if res.failed > 0 || res.interrupted {
		log.Printf("not finished — re-run to retry (re-running is safe and skips what is done)")
		os.Exit(1)
	}
}

// imageRows is the slice of store.MediaStore this command uses, so tests
// can drive run without Postgres.
type imageRows interface {
	ListImagesNeedingThumb(ctx context.Context, afterID string, limit int) ([]model.Image, error)
	UpsertImageVariant(ctx context.Context, in store.UpsertImageVariantInput) (string, error)
}

type options struct {
	dryRun bool
	limit  int // 0 = no limit
	batch  int
	out    io.Writer // dry-run listing
}

type result struct {
	seen        int // images selected
	done        int // images fully processed
	failed      int
	interrupted bool
}

// run walks the images that need work, batch by batch, in id order.
//
// The cursor advances past every image it selects, including ones that
// fail, so one bad object cannot stall a run in a loop; the predicate is what
// brings failures back on the next run. A dry run uses the same cursor
// because it writes nothing, so without one it would re-read the first batch
// forever.
func run(ctx context.Context, rows imageRows, ms media.Store, opts options) (result, error) {
	var res result
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			res.interrupted = true
			return res, nil
		}
		n := opts.batch
		if opts.limit > 0 {
			remaining := opts.limit - res.seen
			if remaining <= 0 {
				return res, nil
			}
			n = min(n, remaining)
		}

		imgs, err := rows.ListImagesNeedingThumb(ctx, cursor, n)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				res.interrupted = true
				return res, nil
			}
			return res, err
		}
		if len(imgs) == 0 {
			return res, nil
		}

		for _, img := range imgs {
			cursor = img.ID
			res.seen++
			if opts.dryRun {
				fmt.Fprintf(opts.out, "%s\t%s\n", img.ID, img.StorageKey)
				continue
			}
			if err := backfillOne(ctx, rows, ms, img); err != nil {
				if errors.Is(err, context.Canceled) {
					res.interrupted = true
					return res, nil
				}
				log.Printf("image %s (%s): %v", img.ID, img.StorageKey, err)
				res.failed++
				continue
			}
			res.done++
			if res.done%200 == 0 {
				log.Printf("  %d processed", res.done)
			}
		}
	}
}

// backfillOne records the full rendition's hash and writes the thumb for
// one image.
//
// The full row is upserted from exercise_images, which is the
// authoritative description of the full rendition (model.Image.StorageKey),
// so an image with no full row (an app upload made before uploads wrote
// one) gains it here with exactly the values the contract already
// synthesizes for it. The hash comes from the bytes just read, so it is
// correct even if a stale one was recorded before.
func backfillOne(ctx context.Context, rows imageRows, ms media.Store, img model.Image) error {
	rc, err := ms.Get(ctx, img.StorageKey)
	if err != nil {
		return fmt.Errorf("read full image: %w", err)
	}
	full, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return fmt.Errorf("read full image: %w", err)
	}

	fullHash := media.ContentSHA256(full)
	if _, err := rows.UpsertImageVariant(ctx, store.UpsertImageVariantInput{
		ImageID:       img.ID,
		Name:          model.VariantFull,
		StorageKey:    img.StorageKey,
		ContentType:   img.ContentType,
		Width:         img.Width,
		Height:        img.Height,
		ContentSHA256: &fullHash,
	}); err != nil {
		return err
	}

	thumb, err := media.StoreThumb(ctx, ms, img.StorageKey, full)
	if err != nil {
		return err
	}
	width, height := thumb.Width, thumb.Height
	if _, err := rows.UpsertImageVariant(ctx, store.UpsertImageVariantInput{
		ImageID:       img.ID,
		Name:          model.VariantThumb,
		StorageKey:    thumb.StorageKey,
		ContentType:   thumb.ContentType,
		Width:         &width,
		Height:        &height,
		ContentSHA256: &thumb.ContentSHA256,
	}); err != nil {
		return err
	}
	return nil
}
