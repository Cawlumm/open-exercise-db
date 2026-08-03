// Command import-freeexercisedb performs the Day-1 trusted bootstrap import
// described in docs/DESIGN.md: fetches free-exercise-db's ~800 exercises,
// re-hosts their images, and inserts them as approved exercises +
// exercise_revisions rows with license="Unlicense".
//
// This bypasses the submissions queue deliberately — it is a trusted
// first-party seed, not a community contribution.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/Cawlumm/open-exercise-db/internal/config"
	"github.com/Cawlumm/open-exercise-db/internal/database"
	"github.com/Cawlumm/open-exercise-db/internal/media"
	"github.com/Cawlumm/open-exercise-db/internal/seed"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

// httpTimeout bounds both the dataset fetch and every individual image
// download, so one slow/hanging upstream response can't stall the whole
// ~800-item run indefinitely.
const httpTimeout = 30 * time.Second

func main() {
	dryRun := flag.Bool("dry-run", false, "report what would be imported without writing anything")
	limit := flag.Int("limit", 0, "import at most N items (0 = no limit); for fast iteration")
	sourceURL := flag.String("source", seed.DefaultSourceURL, "free-exercise-db exercises.json URL")
	imageBaseURL := flag.String("image-base", seed.DefaultImageBaseURL, "base URL each item's relative image path resolves against")
	flag.Parse()

	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate database: %v", err)
	}

	st := store.New(pool)

	var mediaStore media.Store
	if !*dryRun {
		// cfg.MediaDir is always absolute now (config.Load resolves it) —
		// logged here so it's obvious which directory this run actually wrote
		// to, rather than depending on whatever directory the command
		// happened to be started from (the exact split that previously sent
		// one run's images to ./data/media and another's to
		// ./backend/data/media with nothing recording which).
		log.Printf("re-hosting images to %s", cfg.MediaDir)
		fsStore, err := media.NewFilesystemStore(cfg.MediaDir)
		if err != nil {
			log.Fatalf("init media store at %s: %v", cfg.MediaDir, err)
		}
		mediaStore = fsStore
	}

	opts := seed.Options{
		SourceURL:    *sourceURL,
		ImageBaseURL: *imageBaseURL,
		HTTPClient:   &http.Client{Timeout: httpTimeout},
		Limit:        *limit,
		DryRun:       *dryRun,
		MediaStore:   mediaStore,
		Logger:       log.Default(),
	}

	result, err := seed.Import(ctx, st, opts)
	if err != nil {
		log.Fatalf("import failed: %v", err)
	}

	mode := "import"
	if *dryRun {
		mode = "dry-run"
	}
	log.Printf(
		"%s complete: fetched=%d created=%d updated=%d unchanged=%d taxonomy_terms=%d images(uploaded=%d reused=%d failed=%d)",
		mode, result.Fetched, result.Created, result.Updated, result.Unchanged, result.TaxonomyTermsSeen,
		result.ImagesUploaded, result.ImagesReused, result.ImagesFailed,
	)
}
