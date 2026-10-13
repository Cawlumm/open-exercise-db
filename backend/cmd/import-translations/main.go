// Command import-translations loads draft machine translations for one locale
// from a JSON file and writes them as origin='machine' rows, the bootstrap
// translation step described in docs/DESIGN.md's seed completeness bar.
//
// The file is an array of store.MachineTranslation. Items are matched by
// source_id and skipped when the exercise's English name or step count has
// changed since the draft was written; human translations are never replaced.
// Run with -dry-run first: it does everything except commit.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Cawlumm/open-exercise-db/internal/config"
	"github.com/Cawlumm/open-exercise-db/internal/database"
	"github.com/Cawlumm/open-exercise-db/internal/store"
)

func main() {
	file := flag.String("file", "", "JSON file of translations (required)")
	locale := flag.String("locale", "", "BCP-47 locale the file is written in, e.g. es (required)")
	dryRun := flag.Bool("dry-run", false, "report what would change, then roll back")
	flag.Parse()
	if *file == "" || *locale == "" {
		flag.Usage()
		os.Exit(2)
	}

	raw, err := os.ReadFile(*file)
	if err != nil {
		log.Fatalf("read %s: %v", *file, err)
	}
	var items []store.MachineTranslation
	if err := json.Unmarshal(raw, &items); err != nil {
		log.Fatalf("decode %s: %v", *file, err)
	}

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

	res, err := store.New(pool).Translations.ImportMachine(ctx, *locale, items, *dryRun)
	if err != nil {
		log.Fatalf("import failed: %v", err)
	}
	mode := "import"
	if *dryRun {
		mode = "dry-run (rolled back)"
	}
	log.Printf("%s complete: items=%d inserted=%d replaced=%d unchanged=%d kept_human=%d stale=%d step_mismatch=%d not_found=%d",
		mode, len(items), res.Inserted, res.Replaced, res.Unchanged, res.KeptHuman, res.Stale, res.Mismatched, res.NotFound)
}
