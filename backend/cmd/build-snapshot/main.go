// Command build-snapshot builds the static catalog snapshot from the public
// API. See docs/API_POLICY.md and docs/DEPLOYMENT.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/Cawlumm/open-exercise-db/internal/snapshot"
)

func main() {
	api := flag.String("api", snapshot.DefaultAPI, "base URL of the public API")
	out := flag.String("out", "", "directory to write into (the checked-out catalog branch root)")
	confirm := flag.Bool("confirm-removals", false, "publish even if en shrank to under half the published count")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "build-snapshot: -out is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cfg := &snapshot.Config{API: *api, Out: *out, ConfirmRemovals: *confirm, Log: os.Stderr}
	res, err := snapshot.Build(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "build-snapshot: refusing to publish:", err)
		os.Exit(1)
	}
	for _, l := range res.Locales {
		fmt.Fprintf(os.Stderr, "  %s: %d exercises, %d bytes, %s\n", l.Code, l.Total, res.Bytes[l.File], l.File)
	}
	fmt.Fprintf(os.Stderr, "%d requests\n", cfg.Requests)
	if res.Changed {
		fmt.Println("changed", res.GeneratedAt)
	} else {
		fmt.Println("unchanged")
	}
}
